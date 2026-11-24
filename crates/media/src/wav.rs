use std::io::{self, Read, Write};

use ambivox_dsp::kernels;

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.to_string())
}

pub fn write(writer: &mut impl Write, samples: &[f32], sample_rate: u32) -> io::Result<()> {
    let mut pcm = vec![0i16; samples.len()];
    kernels::f32_to_pcm16(samples, &mut pcm);
    let data_len = u32::try_from(pcm.len() * 2).map_err(|_| invalid("audio too long for a WAV file"))?;

    writer.write_all(b"RIFF")?;
    writer.write_all(&(36 + data_len).to_le_bytes())?;
    writer.write_all(b"WAVEfmt ")?;
    writer.write_all(&16u32.to_le_bytes())?;
    writer.write_all(&1u16.to_le_bytes())?;
    writer.write_all(&1u16.to_le_bytes())?;
    writer.write_all(&sample_rate.to_le_bytes())?;
    writer.write_all(&(sample_rate * 2).to_le_bytes())?;
    writer.write_all(&2u16.to_le_bytes())?;
    writer.write_all(&16u16.to_le_bytes())?;
    writer.write_all(b"data")?;
    writer.write_all(&data_len.to_le_bytes())?;
    let bytes: Vec<u8> = pcm.iter().flat_map(|s| s.to_le_bytes()).collect();
    writer.write_all(&bytes)
}

pub fn read(reader: &mut impl Read) -> io::Result<(Vec<f32>, u32)> {
    let mut bytes = Vec::new();
    reader.read_to_end(&mut bytes)?;
    if bytes.len() < 12 || &bytes[..4] != b"RIFF" || &bytes[8..12] != b"WAVE" {
        return Err(invalid("not a RIFF/WAVE file"));
    }

    let mut format: Option<(u16, u16, u32, u16)> = None;
    let mut offset = 12;
    while offset + 8 <= bytes.len() {
        let id = &bytes[offset..offset + 4];
        let size = u32::from_le_bytes(bytes[offset + 4..offset + 8].try_into().unwrap()) as usize;
        let body = offset + 8;
        let end = body.saturating_add(size).min(bytes.len());
        match id {
            b"fmt " if size >= 16 && end - body >= 16 => {
                let field16 = |at: usize| u16::from_le_bytes(bytes[body + at..body + at + 2].try_into().unwrap());
                let rate = u32::from_le_bytes(bytes[body + 4..body + 8].try_into().unwrap());
                format = Some((field16(0), field16(2), rate, field16(14)));
            }
            b"data" => {
                let (tag, channels, rate, bits) = format.ok_or_else(|| invalid("data chunk before fmt chunk"))?;
                if tag != 1 || channels != 1 || bits != 16 || rate == 0 {
                    return Err(invalid("only 16-bit mono PCM is supported"));
                }
                let pcm: Vec<i16> =
                    bytes[body..end].as_chunks::<2>().0.iter().map(|b| i16::from_le_bytes(*b)).collect();
                let mut samples = vec![0.0f32; pcm.len()];
                kernels::pcm16_to_f32(&pcm, &mut samples);
                return Ok((samples, rate));
            }
            _ => {}
        }
        offset = body.saturating_add(size).saturating_add(size & 1);
    }
    Err(invalid("no data chunk"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let samples: Vec<f32> = (0..1000).map(|i| ((i as f32) * 0.05).sin() * 0.8).collect();
        let mut file = Vec::new();
        write(&mut file, &samples, 22_050).unwrap();
        assert_eq!(file.len(), 44 + 2000);

        let (restored, rate) = read(&mut file.as_slice()).unwrap();
        assert_eq!(rate, 22_050);
        assert_eq!(restored.len(), samples.len());
        for (a, b) in samples.iter().zip(&restored) {
            assert!((a - b).abs() <= 1.0 / 32768.0);
        }
    }

    #[test]
    fn unknown_chunks_are_skipped() {
        let mut file = Vec::new();
        write(&mut file, &[0.25, -0.25], 16_000).unwrap();
        let mut padded = file[..12].to_vec();
        padded.extend_from_slice(b"LIST\x03\x00\x00\x00abc\x00");
        padded.extend_from_slice(&file[12..]);
        let (restored, rate) = read(&mut padded.as_slice()).unwrap();
        assert_eq!((restored, rate), (vec![0.25, -0.25], 16_000));
    }

    #[test]
    fn rejects_what_it_cannot_read() {
        assert!(read(&mut &b"not a wav file at all"[..]).is_err());

        let mut stereo = Vec::new();
        write(&mut stereo, &[0.0; 4], 16_000).unwrap();
        stereo[22] = 2;
        assert!(read(&mut stereo.as_slice()).is_err());

        let mut truncated = Vec::new();
        write(&mut truncated, &[0.0; 4], 16_000).unwrap();
        truncated.truncate(30);
        assert!(read(&mut truncated.as_slice()).is_err());
    }
}
