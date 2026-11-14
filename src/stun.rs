//! Minimal STUN (RFC 5389) binding request, used to learn our public address.

use rand::RngCore;
use std::net::{Ipv4Addr, SocketAddr, SocketAddrV4, ToSocketAddrs};
use std::sync::mpsc::Receiver;
use std::time::{Duration, Instant};

pub const STUN_SERVERS: &[&str] = &["stun.l.google.com:19302", "stun1.l.google.com:19302", "stun.cloudflare.com:3478"];
const MAGIC: u32 = 0x2112_A442;

pub fn is_stun(b: &[u8]) -> bool {
    b.len() >= 20 && b[0] & 0xC0 == 0 && u32::from_be_bytes([b[4], b[5], b[6], b[7]]) == MAGIC
}

pub fn request() -> (Vec<u8>, [u8; 12]) {
    let mut txid = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut txid);
    let mut req = vec![0u8; 20];
    req[0..2].copy_from_slice(&1u16.to_be_bytes());
    req[4..8].copy_from_slice(&MAGIC.to_be_bytes());
    req[8..20].copy_from_slice(&txid);
    (req, txid)
}

pub fn parse(b: &[u8], txid: &[u8; 12]) -> Option<SocketAddrV4> {
    if !is_stun(b) || u16::from_be_bytes([b[0], b[1]]) != 0x0101 || &b[8..20] != txid {
        return None;
    }
    let n = u16::from_be_bytes([b[2], b[3]]) as usize;
    let mut attrs = b.get(20..20 + n)?;
    let mut mapped = None;
    while attrs.len() >= 4 {
        let t = u16::from_be_bytes([attrs[0], attrs[1]]);
        let l = u16::from_be_bytes([attrs[2], attrs[3]]) as usize;
        if attrs.len() < 4 + l {
            break;
        }
        let v = &attrs[4..4 + l];
        if (t == 0x0020 || t == 0x0001) && l >= 8 && v[1] == 0x01 {
            let mut port = u16::from_be_bytes([v[2], v[3]]);
            let mut ip = [v[4], v[5], v[6], v[7]];
            if t == 0x0020 {
                port ^= (MAGIC >> 16) as u16;
                let c = MAGIC.to_be_bytes();
                for i in 0..4 {
                    ip[i] ^= c[i];
                }
                return Some(SocketAddrV4::new(Ipv4Addr::from(ip), port));
            }
            mapped = Some(SocketAddrV4::new(Ipv4Addr::from(ip), port));
        }
        let adv = 4 + ((l + 3) & !3);
        if adv > attrs.len() {
            break;
        }
        attrs = &attrs[adv..];
    }
    mapped
}

/// Sends STUN requests through `send`; responses are routed back by the caller's read
/// loop through `rx`. Returns the public address and whether the NAT looks symmetric.
pub fn discover(send: &dyn Fn(&[u8], SocketAddr), rx: &Receiver<Vec<u8>>) -> Result<(SocketAddrV4, bool), String> {
    let mut results: Vec<SocketAddrV4> = Vec::new();
    for srv in STUN_SERVERS {
        let Some(ra) = srv.to_socket_addrs().ok().and_then(|mut i| i.find(|a| a.is_ipv4())) else { continue };
        let (req, txid) = request();
        'attempts: for _ in 0..3 {
            send(&req, ra);
            let deadline = Instant::now() + Duration::from_millis(700);
            loop {
                let left = deadline.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    break;
                }
                match rx.recv_timeout(left) {
                    Ok(b) => {
                        if let Some(a) = parse(&b, &txid) {
                            results.push(a);
                            break 'attempts;
                        }
                    }
                    Err(_) => break,
                }
            }
        }
        if results.len() >= 2 {
            break;
        }
    }
    if results.is_empty() {
        return Err("could not reach any STUN server (no internet, or UDP blocked)".into());
    }
    let symmetric = results.len() >= 2 && results[0].port() != results[1].port();
    Ok((results[0], symmetric))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_xor_mapped() {
        let (_, txid) = request();
        let mut b = vec![0u8; 20];
        b[0..2].copy_from_slice(&0x0101u16.to_be_bytes());
        b[2..4].copy_from_slice(&12u16.to_be_bytes());
        b[4..8].copy_from_slice(&MAGIC.to_be_bytes());
        b[8..20].copy_from_slice(&txid);
        let port = 54321u16 ^ (MAGIC >> 16) as u16;
        let ip = u32::from(Ipv4Addr::new(203, 0, 113, 7)) ^ MAGIC;
        b.extend_from_slice(&0x0020u16.to_be_bytes());
        b.extend_from_slice(&8u16.to_be_bytes());
        b.extend_from_slice(&[0, 1]);
        b.extend_from_slice(&port.to_be_bytes());
        b.extend_from_slice(&ip.to_be_bytes());
        assert_eq!(parse(&b, &txid), Some(SocketAddrV4::new(Ipv4Addr::new(203, 0, 113, 7), 54321)));
        let mut other = txid;
        other[0] ^= 1;
        assert_eq!(parse(&b, &other), None);
    }
}
