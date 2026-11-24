use std::collections::HashMap;
use std::io::{self, BufReader, BufWriter, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::Arc;
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, SyncSender};
use std::thread;
use std::time::{Duration, Instant};

use crate::log;
use crate::pipeline::{Pipeline, Stream};
use crate::protocol::{Frame, VERSION};

const OUTPUT_QUEUE: usize = 4096;
const STATS_INTERVAL: Duration = Duration::from_secs(2);

pub fn serve(listener: UnixListener, pipeline: Arc<Pipeline>) -> io::Result<()> {
    for connection in listener.incoming() {
        let connection = connection?;
        let pipeline = pipeline.clone();
        thread::Builder::new().name("connection".into()).spawn(move || {
            log::info("client connected");
            match handle(connection, pipeline) {
                Ok(()) => log::info("client disconnected"),
                Err(error) => log::warn(&format!("client dropped: {error}")),
            }
        })?;
    }
    Ok(())
}

pub fn handle(connection: UnixStream, pipeline: Arc<Pipeline>) -> io::Result<()> {
    let (output, frames) = mpsc::sync_channel::<Frame>(OUTPUT_QUEUE);
    let writer = {
        let (connection, pipeline) = (connection.try_clone()?, pipeline.clone());
        thread::Builder::new().name("writer".into()).spawn(move || write_loop(connection, frames, &pipeline))?
    };
    let _ = output.send(Frame::Hello { version: VERSION, backend: pipeline.backend().to_string() });

    let result = read_loop(&connection, &pipeline, &output);
    drop(output);
    let _ = connection.shutdown(std::net::Shutdown::Read);
    let _ = writer.join();
    result
}

fn read_loop(connection: &UnixStream, pipeline: &Arc<Pipeline>, output: &SyncSender<Frame>) -> io::Result<()> {
    let mut reader = BufReader::new(connection);
    let mut streams: HashMap<u32, Stream> = HashMap::new();

    while let Some(frame) = Frame::read_from(&mut reader)? {
        match frame {
            Frame::Open { stream, source, target, early } => {
                let opened = if streams.contains_key(&stream) {
                    Err("stream id already in use".to_string())
                } else {
                    pipeline.open(stream, &source, &target, early, output.clone()).map_err(|e| e.to_string())
                };
                match opened {
                    Ok(handle) => {
                        log::info(&format!("stream {stream} opened ({source} -> {target})"));
                        streams.insert(stream, handle);
                    }
                    Err(message) => {
                        log::warn(&format!("stream {stream} refused: {message}"));
                        let _ = output.send(Frame::Error { stream, message });
                        let _ = output.send(Frame::Closed { stream });
                    }
                }
            }
            Frame::Audio { stream, sequence, packet } => {
                if let Some(handle) = streams.get(&stream) {
                    handle.push(sequence, packet);
                }
            }
            Frame::Close { stream } => {
                if let Some(handle) = streams.remove(&stream) {
                    log::info(&format!("stream {stream} closing"));
                    thread::Builder::new().name(format!("close-{stream}")).spawn(move || handle.close())?;
                }
            }
            Frame::Hello { version, .. } if version != VERSION => {
                return Err(io::Error::new(io::ErrorKind::InvalidData, "protocol version mismatch"));
            }
            Frame::Hello { .. } => {}
            _ => return Err(io::Error::new(io::ErrorKind::InvalidData, "unexpected frame from client")),
        }
    }
    Ok(())
}

fn write_loop(connection: UnixStream, frames: Receiver<Frame>, pipeline: &Pipeline) {
    let mut writer = BufWriter::new(connection);
    let mut next_stats = Instant::now() + STATS_INTERVAL;
    loop {
        let frame = match frames.recv_timeout(next_stats.saturating_duration_since(Instant::now())) {
            Ok(frame) => frame,
            Err(RecvTimeoutError::Timeout) => {
                next_stats = Instant::now() + STATS_INTERVAL;
                Frame::Stats { json: pipeline.stats().to_json() }
            }
            Err(RecvTimeoutError::Disconnected) => break,
        };
        if frame.write_to(&mut writer).is_err() {
            break;
        }
        while let Ok(frame) = frames.try_recv() {
            if frame.write_to(&mut writer).is_err() {
                return;
            }
        }
        if writer.flush().is_err() {
            break;
        }
    }
    let _ = writer.flush();
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::codec::{Decoder, Encoder, SAMPLE_RATE};
    use crate::pipeline::Config;
    use crate::protocol::CaptionKind;
    use ambivox_dsp::resample::resample;
    use ambivox_engine::{ASR_SAMPLE_RATE, Engine};

    fn start() -> (UnixStream, thread::JoinHandle<io::Result<()>>) {
        let (client, server) = UnixStream::pair().unwrap();
        let pipeline = Pipeline::new(Arc::new(Engine::sim()), Config { realtime: false, ..Config::default() });
        (client, thread::spawn(move || handle(server, pipeline)))
    }

    fn read(client: &mut UnixStream) -> Frame {
        client.set_read_timeout(Some(Duration::from_secs(20))).unwrap();
        Frame::read_from(client).unwrap().expect("connection closed early")
    }

    #[test]
    fn a_full_session_over_the_socket() {
        let engine = Engine::sim();
        let (mut client, server) = start();
        assert_eq!(read(&mut client), Frame::Hello { version: VERSION, backend: "sim".into() });

        Frame::Open { stream: 42, source: "fr".into(), target: "en".into(), early: false }
            .write_to(&mut client)
            .unwrap();
        let audio = engine.synthesize("bonjour ami", "fr").unwrap();
        let mut pcm = vec![0.0; SAMPLE_RATE as usize / 2];
        pcm.extend(resample(&audio.samples, audio.sample_rate, SAMPLE_RATE));
        pcm.resize(pcm.len() + SAMPLE_RATE as usize, 0.0);
        for (i, packet) in Encoder::new(32_000).unwrap().encode_all(&pcm).unwrap().into_iter().enumerate() {
            Frame::Audio { stream: 42, sequence: i as u16, packet }.write_to(&mut client).unwrap();
        }
        Frame::Close { stream: 42 }.write_to(&mut client).unwrap();

        let mut captions = Vec::new();
        let mut decoder = Decoder::new().unwrap();
        let mut speech = Vec::new();
        loop {
            match read(&mut client) {
                Frame::Caption { stream: 42, kind, .. } if kind.is_partial() => {}
                Frame::Caption { stream: 42, kind, text, .. } => captions.push((kind, text)),
                Frame::Speech { stream: 42, packet } => decoder.decode(&packet, &mut speech).unwrap(),
                Frame::Closed { stream: 42 } => break,
                Frame::Stats { .. } => {}
                other => panic!("unexpected frame {other:?}"),
            }
        }
        assert_eq!(
            captions,
            vec![
                (CaptionKind::Source, "bonjour ami".to_string()),
                (CaptionKind::Translation, "hello friend".to_string())
            ]
        );
        let heard = engine.transcribe(&resample(&speech, SAMPLE_RATE, ASR_SAMPLE_RATE), "en").unwrap();
        assert_eq!(heard, "hello friend");

        drop(client);
        server.join().unwrap().unwrap();
    }

    #[test]
    fn bad_opens_are_answered_not_fatal() {
        let (mut client, server) = start();
        read(&mut client);

        Frame::Open { stream: 1, source: "en".into(), target: "zz".into(), early: false }
            .write_to(&mut client)
            .unwrap();
        assert!(matches!(read(&mut client), Frame::Error { stream: 1, message } if message.contains("zz")));
        assert_eq!(read(&mut client), Frame::Closed { stream: 1 });

        Frame::Open { stream: 2, source: "en".into(), target: "fr".into(), early: false }
            .write_to(&mut client)
            .unwrap();
        Frame::Open { stream: 2, source: "en".into(), target: "fr".into(), early: false }
            .write_to(&mut client)
            .unwrap();
        assert!(matches!(read(&mut client), Frame::Error { stream: 2, .. }));
        assert_eq!(read(&mut client), Frame::Closed { stream: 2 });

        Frame::Audio { stream: 99, sequence: 0, packet: vec![1, 2, 3] }.write_to(&mut client).unwrap();
        Frame::Close { stream: 99 }.write_to(&mut client).unwrap();
        Frame::Close { stream: 2 }.write_to(&mut client).unwrap();
        assert_eq!(read(&mut client), Frame::Closed { stream: 2 });

        drop(client);
        server.join().unwrap().unwrap();
    }

    #[test]
    fn protocol_violations_drop_the_connection() {
        let (mut client, server) = start();
        read(&mut client);
        Frame::Closed { stream: 1 }.write_to(&mut client).unwrap();
        assert!(server.join().unwrap().is_err());
    }

    #[test]
    fn stats_are_published_periodically() {
        let (mut client, server) = start();
        read(&mut client);
        assert!(matches!(read(&mut client), Frame::Stats { json } if json.contains("\"streams\":0")));
        drop(client);
        server.join().unwrap().unwrap();
    }
}
