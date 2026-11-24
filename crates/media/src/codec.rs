use std::io::{self, Read, Write};

use opus::{Application, Bitrate, Channels};

pub const SAMPLE_RATE: u32 = 48_000;
pub const FRAME_SAMPLES: usize = 960;
pub const FRAME_MS: u64 = 20;
const MAX_PACKET: usize = 1500;
const MAX_FRAME_SAMPLES: usize = 5760;

#[derive(Debug)]
pub struct CodecError(pub String);

impl std::fmt::Display for CodecError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "opus: {}", self.0)
    }
}

impl std::error::Error for CodecError {}

impl From<opus::Error> for CodecError {
    fn from(e: opus::Error) -> Self {
        CodecError(e.to_string())
    }
}

pub struct Encoder {
    inner: opus::Encoder,
}

impl Encoder {
    pub fn new(bitrate: i32) -> Result<Self, CodecError> {
        let mut inner = opus::Encoder::new(SAMPLE_RATE, Channels::Mono, Application::Voip)?;
        inner.set_bitrate(Bitrate::Bits(bitrate))?;
        inner.set_inband_fec(true)?;
        inner.set_packet_loss_perc(5)?;
        Ok(Self { inner })
    }

    pub fn encode(&mut self, frame: &[f32]) -> Result<Vec<u8>, CodecError> {
        assert_eq!(frame.len(), FRAME_SAMPLES);
        let mut packet = vec![0u8; MAX_PACKET];
        let n = self.inner.encode_float(frame, &mut packet)?;
        packet.truncate(n);
        Ok(packet)
    }

    pub fn encode_all(&mut self, pcm: &[f32]) -> Result<Vec<Vec<u8>>, CodecError> {
        let mut frame = [0.0f32; FRAME_SAMPLES];
        pcm.chunks(FRAME_SAMPLES)
            .map(|chunk| {
                frame[..chunk.len()].copy_from_slice(chunk);
                frame[chunk.len()..].fill(0.0);
                self.encode(&frame)
            })
            .collect()
    }
}

pub struct Decoder {
    inner: opus::Decoder,
    scratch: Vec<f32>,
}

impl Decoder {
    pub fn new() -> Result<Self, CodecError> {
        Ok(Self { inner: opus::Decoder::new(SAMPLE_RATE, Channels::Mono)?, scratch: vec![0.0; MAX_FRAME_SAMPLES] })
    }

    pub fn decode(&mut self, packet: &[u8], out: &mut Vec<f32>) -> Result<(), CodecError> {
        let n = self.inner.decode_float(packet, &mut self.scratch, false)?;
        out.extend_from_slice(&self.scratch[..n]);
        Ok(())
    }

    pub fn conceal(&mut self, out: &mut Vec<f32>) -> Result<(), CodecError> {
        let n = self.inner.decode_float(&[], &mut self.scratch[..FRAME_SAMPLES], false)?;
        out.extend_from_slice(&self.scratch[..n]);
        Ok(())
    }
}

pub fn write_packets(writer: &mut impl Write, packets: &[Vec<u8>]) -> io::Result<()> {
    for packet in packets {
        let len = u16::try_from(packet.len())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "packet larger than 64 KiB"))?;
        writer.write_all(&len.to_be_bytes())?;
        writer.write_all(packet)?;
    }
    Ok(())
}

pub fn read_packets(reader: &mut impl Read) -> io::Result<Vec<Vec<u8>>> {
    let mut packets = Vec::new();
    loop {
        let mut len = [0u8; 2];
        match reader.read_exact(&mut len) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => return Ok(packets),
            Err(e) => return Err(e),
        }
        let mut packet = vec![0u8; u16::from_be_bytes(len) as usize];
        reader.read_exact(&mut packet)?;
        packets.push(packet);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tone(hz: f32, frames: usize) -> Vec<f32> {
        (0..frames * FRAME_SAMPLES)
            .map(|i| 0.4 * (2.0 * std::f32::consts::PI * hz * i as f32 / SAMPLE_RATE as f32).sin())
            .collect()
    }

    fn rms(x: &[f32]) -> f32 {
        (x.iter().map(|s| s * s).sum::<f32>() / x.len() as f32).sqrt()
    }

    #[test]
    fn tone_survives_the_codec() {
        let input = tone(1000.0, 50);
        let packets = Encoder::new(32_000).unwrap().encode_all(&input).unwrap();
        assert_eq!(packets.len(), 50);

        let mut decoder = Decoder::new().unwrap();
        let mut output = Vec::new();
        for packet in &packets {
            decoder.decode(packet, &mut output).unwrap();
        }
        assert_eq!(output.len(), input.len());
        let level = rms(&output[output.len() / 2..]);
        assert!((level - 0.283).abs() < 0.03, "rms {level}");
    }

    #[test]
    fn lost_packets_are_concealed() {
        let packets = Encoder::new(32_000).unwrap().encode_all(&tone(600.0, 20)).unwrap();
        let mut decoder = Decoder::new().unwrap();
        let mut output = Vec::new();
        for (i, packet) in packets.iter().enumerate() {
            if i == 10 {
                decoder.conceal(&mut output).unwrap();
            } else {
                decoder.decode(packet, &mut output).unwrap();
            }
        }
        assert_eq!(output.len(), 20 * FRAME_SAMPLES);
    }

    #[test]
    fn garbage_is_an_error_not_a_panic() {
        let mut decoder = Decoder::new().unwrap();
        let mut output = Vec::new();
        let _ = decoder.decode(&[0xff; 7], &mut output);
        let _ = decoder.decode(&[0x00], &mut output);
    }

    #[test]
    fn packet_files_roundtrip() {
        let packets = vec![vec![1, 2, 3], vec![], vec![9; 300]];
        let mut file = Vec::new();
        write_packets(&mut file, &packets).unwrap();
        assert_eq!(read_packets(&mut file.as_slice()).unwrap(), packets);
    }
}
