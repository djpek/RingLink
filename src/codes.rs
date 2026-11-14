//! Room codes and the secrets derived from them.

use argon2::{Algorithm, Argon2, Params, Version};
use rand::RngCore;
use sha2::{Digest, Sha256};

/// Crockford base32: no I, L, O, U.
pub const CODE_ALPHABET: &[u8] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";
pub const CODE_LEN: usize = 8;

pub fn new_room_code() -> String {
    let mut b = [0u8; CODE_LEN];
    rand::thread_rng().fill_bytes(&mut b);
    let s: String = b.iter().map(|x| CODE_ALPHABET[(*x as usize) % 32] as char).collect();
    format_code(&s)
}

/// Accepts what people actually type: lower case, spaces, dashes, O for 0 and I/L for 1.
pub fn normalize_code(s: &str) -> Result<String, String> {
    let mut out = String::new();
    for mut r in s.to_uppercase().chars() {
        match r {
            'O' => r = '0',
            'I' | 'L' => r = '1',
            'U' => r = 'V',
            ' ' | '-' | '_' | '\t' | '\n' | '\r' => continue,
            _ => {}
        }
        if !r.is_ascii() || !CODE_ALPHABET.contains(&(r as u8)) {
            return Err(format!("\"{r}\" isn't used in room codes"));
        }
        out.push(r);
    }
    if out.len() != CODE_LEN {
        return Err(format!("room codes have {CODE_LEN} characters (got {})", out.len()));
    }
    Ok(out)
}

pub fn format_code(c: &str) -> String {
    if c.len() == CODE_LEN {
        format!("{}-{}", &c[..4], &c[4..])
    } else {
        c.to_string()
    }
}

/// Derives the encryption key and the relay topic from a normalized code.
/// Argon2 makes guessing codes from captured relay messages slow.
pub fn room_secrets(code: &str) -> ([u8; 32], String) {
    let params = Params::new(32 * 1024, 2, 2, Some(32)).expect("argon2 params");
    let a2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
    let mut key = [0u8; 32];
    a2.hash_password_into(code.as_bytes(), b"ringlink-room-v5", &mut key).expect("argon2");
    let mut h = Sha256::new();
    h.update(b"ringlink-topic-v5/");
    h.update(key);
    let d = h.finalize();
    (key, format!("rl5_{}", hex::encode(&d[..12])))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_round_trip() {
        for _ in 0..50 {
            let c = new_room_code();
            assert_eq!(c.len(), 9);
            let n = normalize_code(&c).unwrap();
            assert_eq!(format_code(&n), c);
        }
    }

    #[test]
    fn forgiving_typing() {
        assert_eq!(normalize_code("k7qm 29xd").unwrap(), "K7QM29XD");
        assert_eq!(normalize_code("oIlu-0000").unwrap(), "011V0000");
        assert!(normalize_code("K7QM-29X").is_err());
        assert!(normalize_code("K7QM-29X!").is_err());
    }

    #[test]
    fn secrets_are_stable_and_distinct() {
        let (k1, t1) = room_secrets("K7QM29XD");
        let (k2, t2) = room_secrets("K7QM29XD");
        let (k3, t3) = room_secrets("K7QM29XE");
        assert_eq!(k1, k2);
        assert_eq!(t1, t2);
        assert_ne!(k1, k3);
        assert_ne!(t1, t3);
        assert!(t1.starts_with("rl5_") && t1.len() == 28);
    }
}
