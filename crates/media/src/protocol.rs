use std::io::{self, Read, Write};

pub const VERSION: u16 = 3;
pub const MAX_FRAME: usize = 64 * 1024;
const HEADER: usize = 5;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u8)]
pub enum CaptionKind {
    Source = 0,
    Translation = 1,
    SourcePartial = 2,
    TranslationPartial = 3,
}

impl CaptionKind {
    pub fn new(translation: bool, partial: bool) -> Self {
        match (translation, partial) {
            (false, false) => CaptionKind::Source,
            (true, false) => CaptionKind::Translation,
            (false, true) => CaptionKind::SourcePartial,
            (true, true) => CaptionKind::TranslationPartial,
        }
    }

    pub fn is_partial(self) -> bool {
        self as u8 & 2 != 0
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Frame {
    Hello { version: u16, backend: String },
    Open { stream: u32, source: String, target: String, early: bool },
    Audio { stream: u32, sequence: u16, packet: Vec<u8> },
    Close { stream: u32 },
    Speech { stream: u32, packet: Vec<u8> },
    Caption { stream: u32, kind: CaptionKind, utterance: u32, text: String },
    Error { stream: u32, message: String },
    Closed { stream: u32 },
    Stats { json: String },
}

mod tag {
    pub const HELLO: u8 = 0x00;
    pub const OPEN: u8 = 0x01;
    pub const AUDIO: u8 = 0x02;
    pub const CLOSE: u8 = 0x03;
    pub const SPEECH: u8 = 0x81;
    pub const CAPTION: u8 = 0x82;
    pub const ERROR: u8 = 0x83;
    pub const CLOSED: u8 = 0x84;
    pub const STATS: u8 = 0x85;
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.to_string())
}

fn utf8(bytes: &[u8]) -> io::Result<String> {
    String::from_utf8(bytes.to_vec()).map_err(|_| invalid("text is not valid UTF-8"))
}

fn short_string(payload: &[u8]) -> io::Result<(String, &[u8])> {
    let (&len, rest) = payload.split_first().ok_or_else(|| invalid("truncated string"))?;
    if rest.len() < len as usize {
        return Err(invalid("truncated string"));
    }
    let (text, rest) = rest.split_at(len as usize);
    Ok((utf8(text)?, rest))
}

fn put_short_string(out: &mut Vec<u8>, text: &str) -> io::Result<()> {
    let len = u8::try_from(text.len()).map_err(|_| invalid("string longer than 255 bytes"))?;
    out.push(len);
    out.extend_from_slice(text.as_bytes());
    Ok(())
}

impl Frame {
    pub fn encode(&self) -> io::Result<Vec<u8>> {
        let mut out = vec![0u8; 4];
        let (tag, stream) = match self {
            Frame::Hello { .. } => (tag::HELLO, 0),
            Frame::Open { stream, .. } => (tag::OPEN, *stream),
            Frame::Audio { stream, .. } => (tag::AUDIO, *stream),
            Frame::Close { stream } => (tag::CLOSE, *stream),
            Frame::Speech { stream, .. } => (tag::SPEECH, *stream),
            Frame::Caption { stream, .. } => (tag::CAPTION, *stream),
            Frame::Error { stream, .. } => (tag::ERROR, *stream),
            Frame::Closed { stream } => (tag::CLOSED, *stream),
            Frame::Stats { .. } => (tag::STATS, 0),
        };
        out.push(tag);
        out.extend_from_slice(&stream.to_be_bytes());
        match self {
            Frame::Hello { version, backend } => {
                out.extend_from_slice(&version.to_be_bytes());
                out.extend_from_slice(backend.as_bytes());
            }
            Frame::Open { source, target, early, .. } => {
                put_short_string(&mut out, source)?;
                put_short_string(&mut out, target)?;
                out.push(u8::from(*early));
            }
            Frame::Audio { sequence, packet, .. } => {
                out.extend_from_slice(&sequence.to_be_bytes());
                out.extend_from_slice(packet);
            }
            Frame::Speech { packet, .. } => out.extend_from_slice(packet),
            Frame::Caption { kind, utterance, text, .. } => {
                out.push(*kind as u8);
                out.extend_from_slice(&utterance.to_be_bytes());
                out.extend_from_slice(text.as_bytes());
            }
            Frame::Error { message, .. } => out.extend_from_slice(message.as_bytes()),
            Frame::Stats { json } => out.extend_from_slice(json.as_bytes()),
            Frame::Close { .. } | Frame::Closed { .. } => {}
        }
        let length = out.len() - 4;
        if length > MAX_FRAME {
            return Err(invalid("frame too large"));
        }
        out[..4].copy_from_slice(&(length as u32).to_be_bytes());
        Ok(out)
    }

    pub fn decode(body: &[u8]) -> io::Result<Frame> {
        if body.len() < HEADER {
            return Err(invalid("frame shorter than its header"));
        }
        let stream = u32::from_be_bytes([body[1], body[2], body[3], body[4]]);
        let payload = &body[HEADER..];
        Ok(match body[0] {
            tag::HELLO => {
                if payload.len() < 2 {
                    return Err(invalid("truncated hello"));
                }
                Frame::Hello { version: u16::from_be_bytes([payload[0], payload[1]]), backend: utf8(&payload[2..])? }
            }
            tag::OPEN => {
                let (source, rest) = short_string(payload)?;
                let (target, rest) = short_string(rest)?;
                let [flags] = rest else {
                    return Err(invalid("open needs exactly one flags byte"));
                };
                if flags & !1 != 0 {
                    return Err(invalid("unknown open flags"));
                }
                Frame::Open { stream, source, target, early: flags & 1 != 0 }
            }
            tag::AUDIO => {
                if payload.len() < 2 {
                    return Err(invalid("truncated audio"));
                }
                Frame::Audio {
                    stream,
                    sequence: u16::from_be_bytes([payload[0], payload[1]]),
                    packet: payload[2..].to_vec(),
                }
            }
            tag::CLOSE => Frame::Close { stream },
            tag::SPEECH => Frame::Speech { stream, packet: payload.to_vec() },
            tag::CAPTION => {
                if payload.len() < 5 {
                    return Err(invalid("truncated caption"));
                }
                if payload[0] > 3 {
                    return Err(invalid("unknown caption kind"));
                }
                let kind = CaptionKind::new(payload[0] & 1 != 0, payload[0] & 2 != 0);
                Frame::Caption {
                    stream,
                    kind,
                    utterance: u32::from_be_bytes([payload[1], payload[2], payload[3], payload[4]]),
                    text: utf8(&payload[5..])?,
                }
            }
            tag::ERROR => Frame::Error { stream, message: utf8(payload)? },
            tag::CLOSED => Frame::Closed { stream },
            tag::STATS => Frame::Stats { json: utf8(payload)? },
            _ => return Err(invalid("unknown frame type")),
        })
    }

    pub fn write_to(&self, writer: &mut impl Write) -> io::Result<()> {
        writer.write_all(&self.encode()?)
    }

    pub fn read_from(reader: &mut impl Read) -> io::Result<Option<Frame>> {
        let mut length = [0u8; 4];
        match reader.read_exact(&mut length) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => return Ok(None),
            Err(e) => return Err(e),
        }
        let length = u32::from_be_bytes(length) as usize;
        if !(HEADER..=MAX_FRAME).contains(&length) {
            return Err(invalid("frame length out of range"));
        }
        let mut body = vec![0u8; length];
        reader.read_exact(&mut body)?;
        Frame::decode(&body).map(Some)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const VECTORS: &str = include_str!("../../../testdata/protocol_vectors.txt");

    fn samples() -> Vec<(&'static str, Frame)> {
        vec![
            ("hello", Frame::Hello { version: 3, backend: "sim".into() }),
            ("open", Frame::Open { stream: 7, source: "en".into(), target: "fr".into(), early: true }),
            ("audio", Frame::Audio { stream: 7, sequence: 65535, packet: vec![0xde, 0xad, 0xbe, 0xef] }),
            ("close", Frame::Close { stream: 7 }),
            ("speech", Frame::Speech { stream: 0x01020304, packet: vec![1, 2, 3] }),
            (
                "caption",
                Frame::Caption { stream: 9, kind: CaptionKind::Translation, utterance: 3, text: "voix à voix".into() },
            ),
            (
                "partial",
                Frame::Caption { stream: 9, kind: CaptionKind::SourcePartial, utterance: 4, text: "voix à".into() },
            ),
            ("error", Frame::Error { stream: 9, message: "unsupported: xx".into() }),
            ("closed", Frame::Closed { stream: 9 }),
            ("stats", Frame::Stats { json: "{\"streams\":1}".into() }),
        ]
    }

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().map(|b| format!("{b:02x}")).collect()
    }

    #[test]
    fn frames_match_the_shared_vectors() {
        let expected: Vec<(&str, &str)> = VECTORS
            .lines()
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
            .map(|l| l.split_once(' ').unwrap())
            .collect();
        let actual = samples();
        assert_eq!(expected.len(), actual.len());
        for ((name, frame), (expected_name, expected_hex)) in actual.iter().zip(expected) {
            assert_eq!(*name, expected_name);
            assert_eq!(hex(&frame.encode().unwrap()), expected_hex, "{name}");
        }
    }

    #[test]
    fn roundtrip_through_a_stream() {
        let mut wire = Vec::new();
        for (_, frame) in samples() {
            frame.write_to(&mut wire).unwrap();
        }
        let mut reader = wire.as_slice();
        for (_, frame) in samples() {
            assert_eq!(Frame::read_from(&mut reader).unwrap(), Some(frame));
        }
        assert_eq!(Frame::read_from(&mut reader).unwrap(), None);
    }

    #[test]
    fn malformed_frames_are_rejected() {
        assert!(Frame::decode(&[]).is_err());
        assert!(Frame::decode(&[0x7f, 0, 0, 0, 0]).is_err());
        assert!(Frame::decode(&[tag::OPEN, 0, 0, 0, 1, 5, b'e']).is_err());
        assert!(Frame::decode(&[tag::OPEN, 0, 0, 0, 1, 2, b'e', b'n', 2, b'f', b'r']).is_err());
        assert!(Frame::decode(&[tag::OPEN, 0, 0, 0, 1, 2, b'e', b'n', 2, b'f', b'r', 2]).is_err());
        assert!(Frame::decode(&[tag::OPEN, 0, 0, 0, 1, 2, b'e', b'n', 2, b'f', b'r', 0, 0]).is_err());
        assert!(Frame::decode(&[tag::OPEN, 0, 0, 0, 1, 2, b'e', b'n', 2, b'f', b'r', 1]).is_ok());
        assert!(Frame::decode(&[tag::CAPTION, 0, 0, 0, 1, 9, 0, 0, 0, 0]).is_err());
        assert!(Frame::decode(&[tag::CAPTION, 0, 0, 0, 1, 4, 0, 0, 0, 0]).is_err());
        assert!(Frame::decode(&[tag::ERROR, 0, 0, 0, 1, 0xff, 0xfe]).is_err());

        let oversized = ((MAX_FRAME + 1) as u32).to_be_bytes();
        assert!(Frame::read_from(&mut oversized.as_slice()).is_err());
        let truncated = [0, 0, 0, 9, tag::CLOSE, 0, 0];
        assert!(Frame::read_from(&mut truncated.as_slice()).is_err());
    }

    #[test]
    fn oversized_fields_fail_to_encode() {
        let long = "x".repeat(256);
        assert!(Frame::Open { stream: 1, source: long, target: "fr".into(), early: false }.encode().is_err());
        assert!(Frame::Speech { stream: 1, packet: vec![0; MAX_FRAME] }.encode().is_err());
    }
}
