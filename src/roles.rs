//! Host and mods.
//!
//! Whoever made the room is the host. The host can make people mods; the host and mods
//! can mute someone for everyone and change the room rules. There's no server, and
//! everyone in the room has the room key, so these changes are signed: each member has
//! its own Ed25519 key (sent in its announce), the host's key is the first one announced
//! as host, and every client checks the signature before following a change. Changes
//! are passed around by everyone every few seconds, so people who join later get them.
//!
//! Muting is enforced twice: the muted person's RingLink stops sending, and everyone
//! else's drops their voice anyway.

use crate::room::{id_hex, now_ms, parse_id, Announce, Room, PK_CTRL};
use base64::Engine;
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};

#[derive(Serialize, Deserialize, Default)]
struct RoleDoc {
    /// "roles" (who's a mod; host only), "mute" (who's muted) or "room" (room rules)
    k: String,
    /// version: newer replaces older
    v: i64,
    by: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    mods: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    muted: Vec<String>,
    #[serde(default, rename = "pitRadio", skip_serializing_if = "Option::is_none")]
    pit_radio: Option<bool>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    isolate: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    subs: Vec<String>,
}

/// Set by the host or mods, the same for everyone in the room.
#[derive(Clone, Debug, PartialEq)]
pub struct RoomRules {
    /// in the garage everyone hears each other at full volume
    pub pit_radio: bool,
    /// everyone hears only this person (0 = nobody)
    pub isolate: u64,
    /// permanent sub-rooms people can pick for the pits
    pub subs: Vec<String>,
}

impl Default for RoomRules {
    fn default() -> Self {
        Self { pit_radio: true, isolate: 0, subs: vec![] }
    }
}

pub(crate) struct RoleState {
    signing: SigningKey,
    self_id: u64,
    pub host_id: u64,
    host_key: Option<VerifyingKey>,
    pub keys: HashMap<u64, VerifyingKey>,
    pub mods: HashSet<u64>,
    pub muted: HashSet<u64>,
    roles_v: i64,
    mute_v: i64,
    rules_v: i64,
    roles_raw: Option<Vec<u8>>,
    mute_raw: Option<Vec<u8>>,
    rules_raw: Option<Vec<u8>>,
    pub rules: RoomRules,
    last_share: Option<Instant>,
}

impl RoleState {
    pub fn new(self_id: u64, creator: bool) -> Self {
        let signing = SigningKey::generate(&mut rand::rngs::OsRng);
        let vk = signing.verifying_key();
        let mut keys = HashMap::new();
        keys.insert(self_id, vk);
        Self {
            signing,
            self_id,
            host_id: if creator { self_id } else { 0 },
            host_key: creator.then_some(vk),
            keys,
            mods: HashSet::new(),
            muted: HashSet::new(),
            roles_v: 0,
            mute_v: 0,
            rules_v: 0,
            roles_raw: None,
            mute_raw: None,
            rules_raw: None,
            rules: RoomRules::default(),
            last_share: None,
        }
    }

    pub fn pub_key_b64(&self) -> String {
        base64::engine::general_purpose::STANDARD_NO_PAD.encode(self.signing.verifying_key().as_bytes())
    }

    /// Records a member's signing key (the first one seen for an id is pinned); the
    /// first host announced is pinned too.
    pub fn note_announced_key(&mut self, id: u64, a: &Announce) {
        if !self.keys.contains_key(&id) {
            if let Ok(k) = base64::engine::general_purpose::STANDARD_NO_PAD.decode(&a.pk) {
                if let Ok(arr) = <[u8; 32]>::try_from(k.as_slice()) {
                    if let Ok(vk) = VerifyingKey::from_bytes(&arr) {
                        self.keys.insert(id, vk);
                    }
                }
            }
        }
        if a.h && self.host_id == 0 {
            if let Some(k) = self.keys.get(&id) {
                self.host_id = id;
                self.host_key = Some(*k);
            }
        }
    }

    /// The signed docs to pass on (every few seconds, for newcomers).
    pub fn share(&mut self) -> Vec<Vec<u8>> {
        if self.last_share.map(|t| t.elapsed() < Duration::from_secs(4)).unwrap_or(false) {
            return vec![];
        }
        self.last_share = Some(Instant::now());
        [&self.roles_raw, &self.mute_raw, &self.rules_raw].into_iter().flatten().cloned().collect()
    }

    fn can_moderate(&self, id: u64) -> bool {
        (self.host_id != 0 && self.host_id == id) || self.mods.contains(&id)
    }

    fn sign(&self, d: &RoleDoc) -> Vec<u8> {
        let js = serde_json::to_vec(d).unwrap();
        let mut raw = self.signing.sign(&js).to_bytes().to_vec();
        raw.extend(js);
        raw
    }

    /// Checks a signed change and takes it if it's valid and newer.
    pub fn apply(&mut self, raw: &[u8]) -> bool {
        if raw.len() <= 64 {
            return false;
        }
        let (sig, js) = raw.split_at(64);
        let Ok(d) = serde_json::from_slice::<RoleDoc>(js) else { return false };
        let Some(by) = parse_id(&d.by) else { return false };
        let Some(key) = self.keys.get(&by) else { return false };
        let Ok(sig) = Signature::from_slice(sig) else { return false };
        if key.verify(js, &sig).is_err() {
            return false;
        }
        // versions are wall-clock ms: refuse ones from far in the future, which would
        // otherwise block every later change
        if d.v > now_ms() + 10 * 60 * 1000 {
            return false;
        }
        let is_host = self.host_id != 0 && by == self.host_id && self.host_key.as_ref() == Some(key);
        match d.k.as_str() {
            "roles" => {
                if !is_host || d.v <= self.roles_v {
                    return false;
                }
                self.mods = parse_ids(&d.mods);
                self.roles_v = d.v;
                self.roles_raw = Some(raw.to_vec());
            }
            "mute" => {
                if (!is_host && !self.mods.contains(&by)) || d.v <= self.mute_v {
                    return false;
                }
                let mut m = parse_ids(&d.muted);
                m.remove(&self.host_id); // nobody mutes the host
                self.muted = m;
                self.mute_v = d.v;
                self.mute_raw = Some(raw.to_vec());
            }
            "room" => {
                if (!is_host && !self.mods.contains(&by)) || d.v <= self.rules_v {
                    return false;
                }
                self.rules = RoomRules {
                    pit_radio: d.pit_radio.unwrap_or(true),
                    isolate: parse_id(&d.isolate).unwrap_or(0),
                    subs: clean_subs(&d.subs),
                };
                self.rules_v = d.v;
                self.rules_raw = Some(raw.to_vec());
            }
            _ => return false,
        }
        true
    }
}

pub fn clean_subs(l: &[String]) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for s in l {
        let s: String = s.trim().chars().filter(|c| !c.is_control()).take(24).collect();
        if !s.is_empty() && out.len() < 12 && !out.iter().any(|o| o.eq_ignore_ascii_case(&s)) {
            out.push(s);
        }
    }
    out
}

fn hex_list(m: &HashSet<u64>) -> Vec<String> {
    let mut v: Vec<String> = m.iter().map(|i| id_hex(*i)).collect();
    v.sort();
    v
}

fn parse_ids(l: &[String]) -> HashSet<u64> {
    l.iter().filter_map(|s| parse_id(s)).collect()
}

impl Room {
    pub fn host_id(&self) -> u64 {
        self.inner.lock().unwrap().roles.host_id
    }

    pub fn am_host(&self) -> bool {
        self.host_id() == self.id
    }

    /// You're the host or a mod.
    pub fn can_moderate(&self) -> bool {
        self.inner.lock().unwrap().roles.can_moderate(self.id)
    }

    /// `id` was muted for everyone by the host or a mod.
    pub fn muted_by_host(&self, id: u64) -> bool {
        self.inner.lock().unwrap().roles.muted.contains(&id)
    }

    pub fn rules(&self) -> RoomRules {
        self.inner.lock().unwrap().roles.rules.clone()
    }

    /// Changes the room rules (host and mods).
    pub fn set_rules(&self, rr: RoomRules) -> Result<(), String> {
        let raw = {
            let g = self.inner.lock().unwrap();
            if !g.roles.can_moderate(self.id) {
                return Err("only the host and mods can change this".into());
            }
            g.roles.sign(&RoleDoc {
                k: "room".into(),
                v: now_ms().max(g.roles.rules_v + 1),
                by: id_hex(self.id),
                pit_radio: Some(rr.pit_radio),
                isolate: if rr.isolate != 0 { id_hex(rr.isolate) } else { String::new() },
                subs: clean_subs(&rr.subs),
                ..Default::default()
            })
        };
        self.publish_doc(raw)
    }

    pub fn set_mod(&self, id: u64, on: bool) -> Result<(), String> {
        let raw = {
            let g = self.inner.lock().unwrap();
            if g.roles.host_id != self.id {
                return Err("only the host can make mods".into());
            }
            let mut mods = g.roles.mods.clone();
            if on {
                mods.insert(id);
            } else {
                mods.remove(&id);
            }
            g.roles.sign(&RoleDoc { k: "roles".into(), v: now_ms().max(g.roles.roles_v + 1), by: id_hex(self.id), mods: hex_list(&mods), ..Default::default() })
        };
        self.publish_doc(raw)
    }

    pub fn set_muted(&self, id: u64, on: bool) -> Result<(), String> {
        let raw = {
            let g = self.inner.lock().unwrap();
            if !g.roles.can_moderate(self.id) {
                return Err("only the host and mods can mute people".into());
            }
            if id == g.roles.host_id {
                return Err("the host can't be muted".into());
            }
            let mut muted = g.roles.muted.clone();
            if on {
                muted.insert(id);
            } else {
                muted.remove(&id);
            }
            g.roles.sign(&RoleDoc { k: "mute".into(), v: now_ms().max(g.roles.mute_v + 1), by: id_hex(self.id), muted: hex_list(&muted), ..Default::default() })
        };
        self.publish_doc(raw)
    }

    fn publish_doc(&self, raw: Vec<u8>) -> Result<(), String> {
        if !self.apply_doc(&raw) {
            return Err("change not accepted".into());
        }
        self.broadcast(PK_CTRL, &raw);
        self.changed();
        Ok(())
    }

    pub(crate) fn apply_doc(&self, raw: &[u8]) -> bool {
        let mut g = self.inner.lock().unwrap();
        debug_assert_eq!(g.roles.self_id, self.id);
        g.roles.apply(raw)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ann(rs: &RoleState, host: bool) -> Announce {
        Announce { k: "a".into(), pk: rs.pub_key_b64(), h: host, ..Default::default() }
    }

    #[test]
    fn only_host_and_mods_are_followed() {
        let host = RoleState::new(1, true);
        let mut modr = RoleState::new(2, false);
        let mut guest = RoleState::new(3, false);
        // everyone learns everyone's key; the host is pinned from its announce
        for rs in [&mut modr, &mut guest] {
            rs.note_announced_key(1, &ann(&host, true));
        }
        let (mk, gk) = (ann(&modr, false), ann(&guest, false));
        modr.note_announced_key(3, &gk);
        guest.note_announced_key(2, &mk);

        // a guest can't mute anyone
        let bad = guest.sign(&RoleDoc { k: "mute".into(), v: 5, by: id_hex(3), muted: vec![id_hex(2)], ..Default::default() });
        assert!(!modr.apply(&bad));
        // a guest can't make itself a mod
        let bad = guest.sign(&RoleDoc { k: "roles".into(), v: 5, by: id_hex(3), mods: vec![id_hex(3)], ..Default::default() });
        assert!(!modr.apply(&bad));
        // forged "by host" with the guest's key fails the signature check
        let forged = guest.sign(&RoleDoc { k: "roles".into(), v: 5, by: id_hex(1), mods: vec![id_hex(3)], ..Default::default() });
        assert!(!modr.apply(&forged));

        // the host makes 2 a mod
        let roles = host.sign(&RoleDoc { k: "roles".into(), v: 10, by: id_hex(1), mods: vec![id_hex(2)], ..Default::default() });
        assert!(modr.apply(&roles) && guest.apply(&roles));
        assert!(!guest.apply(&roles), "same version isn't applied twice");
        // now the mod can mute the guest, but never the host
        let mute = modr.sign(&RoleDoc { k: "mute".into(), v: 11, by: id_hex(2), muted: vec![id_hex(3), id_hex(1)], ..Default::default() });
        assert!(guest.apply(&mute));
        assert!(guest.muted.contains(&3) && !guest.muted.contains(&1));
        // and change room rules
        let room = modr.sign(&RoleDoc {
            k: "room".into(),
            v: 12,
            by: id_hex(2),
            pit_radio: Some(false),
            subs: vec!["Team A".into(), "team a".into(), "  ".into(), "B".into()],
            ..Default::default()
        });
        assert!(guest.apply(&room));
        assert_eq!(guest.rules, RoomRules { pit_radio: false, isolate: 0, subs: vec!["Team A".into(), "B".into()] });
        // a second "host" announce doesn't replace the pinned one
        guest.note_announced_key(2, &Announce { h: true, ..mk });
        assert_eq!(guest.host_id, 1);
    }
}
