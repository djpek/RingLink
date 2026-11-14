//! The window.

use crate::app::{App, APP_VERSION};
use crate::audio::{self, AudioDevice};
use crate::bind::{bind_name, ACT_BROADCAST, ACT_MUTE, ACT_PTM, ACT_PTT};
use crate::codes::normalize_code;
use crate::games::BEAMNG_PORT;
use crate::roles::RoomRules;
use crate::room::{MemberInfo, Room, MAX_ROOM_SIZE};
use crate::voice::{VoiceSettings, VoiceStatus};
use egui::{pos2, vec2, Align, Align2, Color32, CornerRadius, FontFamily, FontId, Layout, Margin, Rect, RichText, Sense, Stroke, StrokeKind, Ui, Vec2};
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

const BG: Color32 = Color32::from_rgb(0x05, 0x0c, 0x1a);
const BG2: Color32 = Color32::from_rgb(0x0d, 0x2a, 0x52);
const TEXT: Color32 = Color32::from_rgb(0xe6, 0xf0, 0xff);
const ACCENT: Color32 = Color32::from_rgb(0x3f, 0xd0, 0xff);
const ACCENT_D: Color32 = Color32::from_rgb(0x03, 0x20, 0x2c);
const GOOD: Color32 = Color32::from_rgb(0x5e, 0xe3, 0x9a);
const WARN: Color32 = Color32::from_rgb(0xf5, 0xc4, 0x51);
const BAD: Color32 = Color32::from_rgb(0xff, 0x7b, 0x7b);
const AVATARS: [Color32; 8] = [
    Color32::from_rgb(0x6c, 0xc4, 0xf5),
    Color32::from_rgb(0xb1, 0x8c, 0xf7),
    Color32::from_rgb(0xf5, 0x9e, 0x6c),
    Color32::from_rgb(0x45, 0xd4, 0x83),
    Color32::from_rgb(0xf0, 0x6c, 0x9b),
    Color32::from_rgb(0xf0, 0xc8, 0x4a),
    Color32::from_rgb(0x5e, 0xd6, 0xc9),
    Color32::from_rgb(0x8e, 0xa2, 0xff),
];

/// What cards are painted with; translucent-looking tints are mixed over this ahead of
/// time and drawn opaque, so they look the same on every graphics driver.
const CARD: Color32 = Color32::from_rgb(0x0f, 0x1c, 0x30);

fn mix(base: Color32, c: Color32, a: u8) -> Color32 {
    let t = a as f32 / 255.0;
    let f = |x: u8, y: u8| (x as f32 + (y as f32 - x as f32) * t).round() as u8;
    Color32::from_rgb(f(base.r(), c.r()), f(base.g(), c.g()), f(base.b(), c.b()))
}
fn muted() -> Color32 {
    mix(CARD, Color32::from_rgb(235, 240, 246), 0xb8)
}
fn faint() -> Color32 {
    mix(CARD, Color32::from_rgb(235, 240, 246), 0x80)
}
fn white(a: u8) -> Color32 {
    mix(CARD, Color32::from_rgb(0xa8, 0xcc, 0xff), a)
}
fn black(a: u8) -> Color32 {
    mix(CARD, Color32::BLACK, a)
}
fn with_alpha(c: Color32, a: u8) -> Color32 {
    mix(CARD, c, a)
}
/// really see-through (backdrop, shadows)
fn alpha(c: Color32, a: u8) -> Color32 {
    Color32::from_rgba_unmultiplied(c.r(), c.g(), c.b(), a)
}
fn bold(size: f32) -> FontId {
    FontId::new(size, FontFamily::Name("bold".into()))
}
fn semi(size: f32) -> FontId {
    FontId::new(size, FontFamily::Name("semi".into()))
}

#[derive(Clone, Copy, PartialEq)]
enum Btn {
    Primary,
    Soft,
    Ghost,
    Danger,
}

pub struct RingLinkUi {
    app: Arc<App>,
    logo: Option<egui::TextureHandle>,
    name: String,
    code: String,
    sub_new: String,
    err: String,
    /// member whose ⋮ menu is open
    menu_for: Option<u64>,
    mics: Vec<AudioDevice>,
    spks: Vec<AudioDevice>,
    dev_at: Option<Instant>,
    copied_at: Option<Instant>,
    last_save: Instant,
    dirty: bool,
    applied_top: Option<bool>,
}

pub fn fonts(ctx: &egui::Context) {
    let mut f = egui::FontDefinitions::default();
    for (name, bytes) in [
        ("sn-regular", &include_bytes!("../assets/SNPro-Regular.ttf")[..]),
        ("sn-semi", &include_bytes!("../assets/SNPro-SemiBold.ttf")[..]),
        ("sn-bold", &include_bytes!("../assets/SNPro-Bold.ttf")[..]),
    ] {
        f.font_data.insert(name.into(), Arc::new(egui::FontData::from_static(bytes)));
    }
    let fallback = f.families.get(&FontFamily::Proportional).cloned().unwrap_or_default();
    for (fam, first) in [(FontFamily::Proportional, "sn-regular"), (FontFamily::Name("semi".into()), "sn-semi"), (FontFamily::Name("bold".into()), "sn-bold")] {
        let mut v = vec![first.to_string()];
        v.extend(fallback.iter().cloned());
        f.families.insert(fam, v);
    }
    ctx.set_fonts(f);
}

fn style(ctx: &egui::Context) {
    let mut st = (*ctx.style()).clone();
    st.visuals = egui::Visuals::dark();
    st.visuals.override_text_color = Some(TEXT);
    st.visuals.panel_fill = Color32::TRANSPARENT;
    st.visuals.window_fill = Color32::from_rgb(0x0c, 0x18, 0x2b);
    st.visuals.selection.bg_fill = with_alpha(ACCENT, 0x70);
    st.visuals.selection.stroke = Stroke::new(1.0_f32, ACCENT);
    st.visuals.extreme_bg_color = black(0x40);
    st.visuals.window_shadow.color = alpha(Color32::BLACK, 0x60);
    st.visuals.popup_shadow.color = alpha(Color32::BLACK, 0x60);
    st.visuals.widgets.inactive.bg_fill = white(0x26);
    st.visuals.widgets.inactive.weak_bg_fill = white(0x14);
    st.visuals.widgets.hovered.bg_fill = white(0x3a);
    st.visuals.widgets.hovered.weak_bg_fill = white(0x26);
    st.visuals.widgets.active.bg_fill = ACCENT;
    st.visuals.widgets.active.weak_bg_fill = white(0x30);
    for w in [&mut st.visuals.widgets.inactive, &mut st.visuals.widgets.hovered, &mut st.visuals.widgets.active, &mut st.visuals.widgets.noninteractive, &mut st.visuals.widgets.open] {
        w.corner_radius = CornerRadius::same(10);
        w.bg_stroke = Stroke::new(1.0_f32, white(0x1c));
        w.fg_stroke = Stroke::new(1.0_f32, TEXT);
    }
    st.visuals.widgets.noninteractive.bg_stroke = Stroke::new(1.0_f32, white(0x1c));
    st.spacing.item_spacing = vec2(8.0, 8.0);
    st.spacing.slider_width = 200.0;
    st.spacing.interact_size.y = 26.0;
    st.spacing.scroll.floating = true;
    st.text_styles.insert(egui::TextStyle::Body, FontId::proportional(14.5));
    st.text_styles.insert(egui::TextStyle::Button, semi(14.5));
    st.text_styles.insert(egui::TextStyle::Small, FontId::proportional(12.5));
    st.text_styles.insert(egui::TextStyle::Heading, bold(22.0));
    ctx.set_style(st);
}

impl RingLinkUi {
    pub fn new(cc: &eframe::CreationContext<'_>, app: Arc<App>) -> Self {
        fonts(&cc.egui_ctx);
        style(&cc.egui_ctx);
        let logo = decode_png(include_bytes!("../assets/logo.png")).map(|(w, h, rgba)| {
            cc.egui_ctx.load_texture("logo", egui::ColorImage::from_rgba_unmultiplied([w as usize, h as usize], &rgba), egui::TextureOptions::LINEAR)
        });
        let name = app.settings.lock().unwrap().display_name.clone();
        Self {
            app,
            logo,
            name,
            code: String::new(),
            sub_new: String::new(),
            err: String::new(),
            menu_for: None,
            mics: vec![],
            spks: vec![],
            dev_at: None,
            copied_at: None,
            last_save: Instant::now(),
            dirty: false,
            applied_top: None,
        }
    }

    fn opacity(&self) -> (f32, f32) {
        let s = self.app.settings.lock().unwrap();
        ((s.opacity.clamp(20, 100)) as f32 / 100.0, (s.panel_opacity.clamp(20, 100)) as f32 / 100.0)
    }

    fn see_through(&self) -> bool {
        self.opacity().0 < 0.995
    }

    fn card_fill(&self) -> Color32 {
        if self.see_through() {
            let a = self.opacity().1;
            alpha(CARD, (a * 255.0) as u8)
        } else {
            CARD
        }
    }

    // ---------- building blocks ----------

    fn card(&self, ui: &mut Ui, icon: Icon, title: &str, trailing: impl FnOnce(&mut Ui), body: impl FnOnce(&mut Ui)) {
        egui::Frame::new()
            .fill(self.card_fill())
            .stroke(Stroke::new(1.0_f32, white(0x2e)))
            .corner_radius(CornerRadius::same(20))
            .inner_margin(Margin::same(18))
            .shadow(egui::Shadow { offset: [0, 6], blur: 18, spread: 0, color: alpha(Color32::BLACK, 0x50) })
            .show(ui, |ui| {
                ui.set_width(ui.available_width());
                ui.horizontal(|ui| {
                    draw_icon(ui, icon, ACCENT, 18.0);
                    ui.label(RichText::new(title).font(semi(17.0)));
                    ui.with_layout(Layout::right_to_left(Align::Center), trailing);
                });
                ui.add_space(4.0);
                body(ui);
            });
        ui.add_space(6.0);
    }

    fn caption(ui: &mut Ui, s: &str) {
        ui.label(RichText::new(s).size(12.5).color(muted()));
    }

    fn section(ui: &mut Ui, s: &str) {
        ui.label(RichText::new(s.to_uppercase()).font(semi(11.5)).color(faint()).extra_letter_spacing(0.6));
    }

    fn pill(ui: &mut Ui, s: &str, c: Color32) {
        egui::Frame::new().fill(with_alpha(c, 0x30)).corner_radius(CornerRadius::same(12)).inner_margin(Margin::symmetric(10, 4)).show(ui, |ui| {
            ui.horizontal(|ui| {
                ui.spacing_mut().item_spacing.x = 6.0;
                let (r, _) = ui.allocate_exact_size(vec2(7.0, 7.0), Sense::hover());
                ui.painter().circle_filled(r.center(), 3.5, c);
                ui.label(RichText::new(s).size(12.5).color(c));
            });
        });
    }

    fn button(ui: &mut Ui, label: &str, kind: Btn, full: bool) -> egui::Response {
        let (bg, fg) = match kind {
            Btn::Primary => (ACCENT, ACCENT_D),
            Btn::Soft => (white(0x1c), TEXT),
            Btn::Ghost => (CARD, muted()),
            Btn::Danger => (with_alpha(BAD, 0x38), BAD),
        };
        let galley = ui.painter().layout_no_wrap(label.to_string(), semi(14.5), fg);
        let w = if full { ui.available_width() } else { galley.size().x + 32.0 };
        let (rect, resp) = ui.allocate_exact_size(vec2(w, 38.0), Sense::click());
        let t = ui.ctx().animate_bool(resp.id, resp.hovered());
        let mut fill = bg;
        if kind == Btn::Primary {
            fill = lerp_c(bg, Color32::from_rgb(0x8f, 0xd3, 0xfa), t);
        } else {
            fill = lerp_c(fill, white(if kind == Btn::Ghost { 0x18 } else { 0x30 }), t * 0.8);
        }
        let rect = if resp.is_pointer_button_down_on() { rect.shrink(1.0) } else { rect };
        ui.painter().rect_filled(rect, 19.0, fill);
        if kind == Btn::Soft {
            ui.painter().rect_stroke(rect, 19.0, Stroke::new(1.0_f32, white(0x1c)), StrokeKind::Inside);
        }
        ui.painter().galley(rect.center() - galley.size() / 2.0, galley, fg);
        resp.on_hover_cursor(egui::CursorIcon::PointingHand)
    }

    /// On / off switch; returns true when clicked.
    fn switch(ui: &mut Ui, on: bool) -> bool {
        let (rect, resp) = ui.allocate_exact_size(vec2(44.0, 24.0), Sense::click());
        let t = ui.ctx().animate_bool(resp.id, on);
        ui.painter().rect_filled(rect, 12.0, lerp_c(white(0x26), ACCENT, t));
        let x = egui::lerp(rect.left() + 12.0..=rect.right() - 12.0, t);
        ui.painter().circle_filled(pos2(x, rect.center().y), 9.0, Color32::WHITE);
        resp.on_hover_cursor(egui::CursorIcon::PointingHand).clicked()
    }

    fn switch_row(ui: &mut Ui, label: &str, help: &str, on: bool) -> bool {
        let mut clicked = false;
        ui.horizontal(|ui| {
            ui.label(RichText::new(label).font(semi(14.5)));
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| clicked = Self::switch(ui, on));
        });
        if !help.is_empty() {
            Self::caption(ui, help);
        }
        clicked
    }

    fn switch_atomic(ui: &mut Ui, label: &str, help: &str, a: &AtomicBool) -> bool {
        if Self::switch_row(ui, label, help, a.load(Ordering::Relaxed)) {
            a.fetch_xor(true, Ordering::Relaxed);
            return true;
        }
        false
    }

    /// Full-width slider with its name on the left and value on the right.
    fn slider(ui: &mut Ui, name: &str, value: &str, v: &mut f32, range: std::ops::RangeInclusive<f32>, default: f32) -> bool {
        let mut reset = false;
        ui.horizontal(|ui| {
            let r = ui.add(egui::Label::new(RichText::new(name).font(semi(14.5))).sense(Sense::click()));
            reset = r.on_hover_text("Double-click to reset").double_clicked();
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| ui.label(RichText::new(value).font(semi(14.0)).color(ACCENT)));
        });
        if reset {
            *v = default;
        }
        let (rect, resp) = ui.allocate_exact_size(vec2(ui.available_width(), 22.0), Sense::click_and_drag());
        let (lo, hi) = (*range.start(), *range.end());
        let track = Rect::from_min_max(pos2(rect.left() + 9.0, rect.center().y - 2.0), pos2(rect.right() - 9.0, rect.center().y + 2.0));
        let mut changed = reset;
        if let (true, Some(p)) = (resp.dragged() || resp.clicked(), resp.interact_pointer_pos()) {
            let t = ((p.x - track.left()) / track.width()).clamp(0.0, 1.0);
            let nv = (lo + t * (hi - lo)).round();
            if nv != *v {
                *v = nv;
                changed = true;
            }
        }
        let t = ((*v - lo) / (hi - lo)).clamp(0.0, 1.0);
        let x = track.left() + t * track.width();
        ui.painter().rect_filled(track, 2.0, white(0x26));
        ui.painter().rect_filled(Rect::from_min_max(track.min, pos2(x, track.bottom())), 2.0, ACCENT);
        let grow = ui.ctx().animate_bool(resp.id, resp.hovered() || resp.dragged());
        ui.painter().circle_filled(pos2(x, rect.center().y), 7.0 + 2.0 * grow, ACCENT);
        ui.painter().circle_filled(pos2(x, rect.center().y), 3.0, Color32::WHITE);
        changed
    }

    fn slider_atomic(ui: &mut Ui, name: &str, value: &str, a: &AtomicI32, range: std::ops::RangeInclusive<f32>, default: f32) -> bool {
        let mut v = a.load(Ordering::Relaxed) as f32;
        let ch = Self::slider(ui, name, value, &mut v, range, default);
        if ch {
            a.store(v as i32, Ordering::Relaxed);
        }
        ch
    }

    fn bar(ui: &mut Ui, frac: f32, c: Color32, w: f32) {
        let (rect, _) = ui.allocate_exact_size(vec2(w, 6.0), Sense::hover());
        ui.painter().rect_filled(rect, 3.0, white(0x22));
        let mut f = rect;
        f.set_width(rect.width() * frac.clamp(0.0, 1.0));
        ui.painter().rect_filled(f, 3.0, c);
    }

    fn segmented(ui: &mut Ui, options: &[&str], sel: usize) -> Option<usize> {
        let mut out = None;
        let (rect, _) = ui.allocate_exact_size(vec2(ui.available_width(), 40.0), Sense::hover());
        ui.painter().rect_filled(rect, 20.0, black(0x26));
        ui.painter().rect_stroke(rect, 20.0, Stroke::new(1.0_f32, white(0x1c)), StrokeKind::Inside);
        let w = (rect.width() - 6.0) / options.len() as f32;
        let x = ui.ctx().animate_value_with_time(ui.id().with("seg").with(options[0]), sel as f32, 0.15);
        let pillr = Rect::from_min_size(pos2(rect.left() + 3.0 + x * w, rect.top() + 3.0), vec2(w, rect.height() - 6.0));
        ui.painter().rect_filled(pillr, 17.0, ACCENT);
        for (i, o) in options.iter().enumerate() {
            let r = Rect::from_min_size(pos2(rect.left() + 3.0 + i as f32 * w, rect.top() + 3.0), vec2(w, rect.height() - 6.0));
            let resp = ui.interact(r, ui.id().with(("seg", *o)), Sense::click());
            let c = if i == sel { ACCENT_D } else { muted() };
            ui.painter().text(r.center(), Align2::CENTER_CENTER, *o, semi(13.5), c);
            if resp.on_hover_cursor(egui::CursorIcon::PointingHand).clicked() && i != sel {
                out = Some(i);
            }
        }
        out
    }

    fn avatar(ui: &mut Ui, name: &str, talking: bool, size: f32) {
        let (rect, _) = ui.allocate_exact_size(Vec2::splat(size), Sense::hover());
        let c = avatar_color(name);
        let t = ui.ctx().animate_bool(ui.id().with(("talk", name)), talking);
        ui.painter().circle_filled(rect.center(), size / 2.0 - 2.0, with_alpha(c, 0x48));
        ui.painter().circle_stroke(rect.center(), size / 2.0 - 1.0, Stroke::new(2.0_f32, lerp_c(with_alpha(c, 0x48), GOOD, t)));
        ui.painter().text(rect.center(), Align2::CENTER_CENTER, initials(name), bold(size * 0.34), c);
    }

    fn text_field(ui: &mut Ui, text: &mut String, hint: &str, mono: bool, width: f32) -> egui::Response {
        let mut out = None;
        egui::Frame::new().fill(white(0x14)).stroke(Stroke::new(1.0_f32, white(0x1c))).corner_radius(CornerRadius::same(12)).inner_margin(Margin::symmetric(12, 8)).show(ui, |ui| {
            let mut te = egui::TextEdit::singleline(text).hint_text(RichText::new(hint).color(faint())).frame(false).desired_width(width - 26.0);
            if mono {
                te = te.font(bold(18.0));
            }
            out = Some(ui.add(te));
        });
        out.unwrap()
    }

    fn touch(&mut self) {
        self.dirty = true;
    }

    // ---------- layout ----------

    fn header(&mut self, ui: &mut Ui, room: Option<&Arc<Room>>) {
        ui.horizontal(|ui| {
            if let Some(t) = &self.logo {
                ui.add(egui::Image::new(t).fit_to_exact_size(vec2(46.0, 46.0)));
            }
            ui.vertical(|ui| {
                ui.spacing_mut().item_spacing.y = 0.0;
                ui.label(RichText::new("RingLink").font(bold(22.0)));
                ui.label(RichText::new("Proximity voice chat for sim racing").size(13.0).color(muted()));
            });
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| match room {
                Some(r) => {
                    let (state, max) = r.state();
                    if state == "in room" {
                        Self::pill(ui, &format!("In room · {}/{max}", r.size()), GOOD);
                    } else {
                        Self::pill(ui, &capitalize(&state), if state.contains("full") { BAD } else { WARN });
                    }
                }
                None => Self::pill(ui, &format!("v{APP_VERSION}"), faint()),
            });
        });
        ui.add_space(8.0);
    }

    fn game_card(&mut self, ui: &mut Ui, vs: &VoiceStatus, in_room: bool) {
        let game = self.app.settings.lock().unwrap().game.clamp(0, 2) as usize;
        let app = self.app.clone();
        self.card(ui, Icon::Car, "Game", |_| {}, |ui| {
            if let Some(g) = Self::segmented(ui, &["Auto", "Le Mans Ultimate", "BeamNG"], game) {
                app.set_game(g as i32);
            }
            Self::caption(
                ui,
                match game {
                    0 => "Uses whichever game you're driving in.",
                    1 => "Reads your car position from Le Mans Ultimate. Nothing to set up.",
                    _ => "Reads your car position from BeamNG.drive / BeamMP.",
                },
            );
            if game != 1 {
                Self::caption(ui, &format!("BeamNG setup: Options › Other › turn on MotionSim, IP 127.0.0.1, port {BEAMNG_PORT}."));
            }
            let (msg, c) = if !in_room {
                ("Join a room to start talking".to_string(), faint())
            } else if !vs.on {
                ("Voice chat is off".to_string(), faint())
            } else if vs.have_pos {
                (format!("On track in {} - voices follow distance", vs.game), GOOD)
            } else if vs.in_garage {
                ("In the garage".to_string(), ACCENT)
            } else {
                ("Not driving - everyone at normal volume".to_string(), faint())
            };
            egui::Frame::new().fill(white(0x12)).corner_radius(CornerRadius::same(13)).inner_margin(Margin::symmetric(10, 5)).show(ui, |ui| {
                ui.set_width(ui.available_width());
                ui.horizontal(|ui| {
                    let (r, _) = ui.allocate_exact_size(vec2(7.0, 7.0), Sense::hover());
                    ui.painter().circle_filled(r.center(), 3.5, c);
                    ui.label(RichText::new(msg).size(12.5).color(if c == faint() { muted() } else { c }));
                });
            });
        });
    }

    fn lobby(&mut self, ui: &mut Ui) {
        let app = self.app.clone();
        let (mut max, last) = {
            let s = app.settings.lock().unwrap();
            (s.max_people as f32, s.last_room.clone())
        };
        let mut create = false;
        let mut join: Option<String> = None;
        let mut name = std::mem::take(&mut self.name);
        let mut code = std::mem::take(&mut self.code);
        let err = self.err.clone();
        let mut changed = false;
        self.card(ui, Icon::People, "Room", |_| {}, |ui| {
            Self::section(ui, "Your name");
            let w = ui.available_width();
            if Self::text_field(ui, &mut name, "Name other people see", false, w).changed() {
                app.settings.lock().unwrap().display_name = name.chars().take(32).collect();
                changed = true;
            }
            ui.add_space(4.0);
            if Self::slider(ui, "Max people", &format!("{}", max as i32), &mut max, 2.0..=MAX_ROOM_SIZE as f32, 4.0) {
                app.settings.lock().unwrap().max_people = max as usize;
                changed = true;
            }
            create = Self::button(ui, "Create room", Btn::Primary, true).clicked();
            ui.add_space(6.0);
            ui.horizontal(|ui| {
                let (l, _) = ui.allocate_exact_size(vec2((ui.available_width() - 30.0) / 2.0, 1.0), Sense::hover());
                ui.painter().rect_filled(l, 0.0, white(0x1c));
                ui.label(RichText::new("or").size(12.5).color(faint()));
                let (l, _) = ui.allocate_exact_size(vec2(ui.available_width(), 1.0), Sense::hover());
                ui.painter().rect_filled(l, 0.0, white(0x1c));
            });
            Self::section(ui, "Join with a code");
            ui.horizontal(|ui| {
                let w = ui.available_width() - 110.0;
                let r = Self::text_field(ui, &mut code, "K7QM-29XD", true, w);
                let enter = r.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter));
                if Self::button(ui, "Join room", Btn::Soft, true).clicked() || enter {
                    join = Some(code.clone());
                }
            });
            if !last.is_empty() && normalize_code(&last).is_ok() {
                ui.horizontal(|ui| {
                    ui.label(RichText::new(format!("Last room: {last}")).size(12.5).color(muted()));
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        if ui.link(RichText::new("Rejoin").font(semi(13.0)).color(ACCENT)).clicked() {
                            join = Some(last.clone());
                        }
                    });
                });
            }
            if !err.is_empty() {
                ui.label(RichText::new(&err).size(13.0).color(BAD));
            }
            Self::caption(ui, "Anyone with the code can join, so only share it with people you want in the room.");
        });
        self.name = name;
        self.code = code;
        if changed {
            self.touch();
        }
        if create {
            self.err = app.create_room().err().map(|e| capitalize(&e)).unwrap_or_default();
        }
        if let Some(c) = join {
            match app.join_room(&c) {
                Ok(()) => {
                    self.err.clear();
                    self.code.clear();
                }
                Err(e) => self.err = capitalize(&e),
            }
        }
    }

    fn in_room(&mut self, ui: &mut Ui, r: &Arc<Room>, vs: &VoiceStatus) {
        let app = self.app.clone();
        let members = r.members();
        let rules = r.rules();
        let (state, max) = r.state();
        let mut leave = false;
        let mut copy = false;
        let copied = self.copied_at.map(|t| t.elapsed() < Duration::from_millis(1500)).unwrap_or(false);
        let mut menu_for = self.menu_for;
        let mut sub_new = std::mem::take(&mut self.sub_new);
        let mut save = false;
        self.card(
            ui,
            Icon::People,
            "Room",
            |ui| leave = Self::button(ui, "Leave", Btn::Danger, false).clicked(),
            |ui| {
                egui::Frame::new().fill(black(0x22)).stroke(Stroke::new(1.0_f32, white(0x1c))).corner_radius(CornerRadius::same(16)).inner_margin(Margin::same(14)).show(ui, |ui| {
                    ui.set_width(ui.available_width());
                    ui.horizontal(|ui| {
                        ui.vertical(|ui| {
                            ui.spacing_mut().item_spacing.y = 2.0;
                            Self::section(ui, "Room code");
                            ui.label(RichText::new(&r.code).font(bold(28.0)).color(ACCENT));
                        });
                        ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                            copy = Self::button(ui, if copied { "Copied" } else { "Copy" }, Btn::Primary, false).clicked();
                        });
                    });
                });
                Self::caption(ui, "Everyone pastes this same code into RingLink to join.");
                if rules.isolate != 0 {
                    let who = members.iter().find(|m| m.id == rules.isolate).map(|m| m.name.clone()).unwrap_or_else(|| "someone".into());
                    egui::Frame::new().fill(with_alpha(WARN, 0x28)).corner_radius(CornerRadius::same(12)).inner_margin(Margin::symmetric(12, 8)).show(ui, |ui| {
                        ui.set_width(ui.available_width());
                        ui.horizontal(|ui| {
                            ui.label(RichText::new(format!("Everyone hears only {who} right now.")).size(13.0).color(WARN));
                            if r.can_moderate() {
                                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                    if ui.link(RichText::new("Stop").font(semi(13.0)).color(WARN)).clicked() {
                                        let _ = r.set_rules(RoomRules { isolate: 0, ..rules.clone() });
                                    }
                                });
                            }
                        });
                    });
                }
                ui.add_space(2.0);
                ui.horizontal(|ui| {
                    Self::section(ui, &format!("People · {} of {max}", members.len()));
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        let others = members.iter().filter(|m| !m.is_self).count();
                        let conn = members.iter().filter(|m| !m.is_self && m.connected).count();
                        if state != "in room" {
                            Self::pill(ui, &capitalize(&state), WARN);
                        } else if others == 0 {
                            Self::pill(ui, "Waiting for people", faint());
                        } else if conn == others {
                            Self::pill(ui, "Connected", GOOD);
                        } else {
                            Self::pill(ui, &format!("Connecting {conn}/{others}"), WARN);
                        }
                    });
                });
                for m in &members {
                    Self::member_row(ui, &app, r, m, vs, &rules, &mut menu_for, &mut save);
                }
                ui.add_space(2.0);
                ui.separator();
                Self::pits(ui, &app, r, &rules, vs, &mut sub_new);
            },
        );
        self.menu_for = menu_for;
        self.sub_new = sub_new;
        if save {
            self.touch();
        }
        if copy {
            if let Ok(mut cb) = arboard::Clipboard::new() {
                let _ = cb.set_text(r.code.clone());
            }
            ui.ctx().copy_text(r.code.clone());
            self.copied_at = Some(Instant::now());
        }
        if leave {
            let a = app.clone();
            std::thread::spawn(move || a.leave_room());
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn member_row(ui: &mut Ui, app: &Arc<App>, r: &Arc<Room>, m: &MemberInfo, vs: &VoiceStatus, rules: &RoomRules, menu_for: &mut Option<u64>, save: &mut bool) {
        let pv = vs.peers.get(&m.id);
        let talking = if m.is_self { vs.talking } else { pv.map(|p| p.talking).unwrap_or(false) };
        ui.horizontal(|ui| {
            Self::avatar(ui, &m.name, talking, 40.0);
            ui.vertical(|ui| {
                ui.spacing_mut().item_spacing.y = 1.0;
                ui.horizontal(|ui| {
                    ui.spacing_mut().item_spacing.x = 6.0;
                    ui.label(RichText::new(if m.is_self { format!("{} (you)", m.name) } else { m.name.clone() }).font(semi(15.0)));
                    if m.host {
                        ui.label(RichText::new("HOST").font(bold(10.0)).color(WARN));
                    } else if m.moderator {
                        ui.label(RichText::new("MOD").font(bold(10.0)).color(ACCENT));
                    }
                    if m.muted {
                        ui.label(RichText::new("muted for everyone").size(11.5).color(BAD));
                    }
                    if pv.map(|p| p.broadcasting).unwrap_or(false) || (m.is_self && app.voice.broadcasting()) {
                        ui.label(RichText::new("BROADCASTING").font(bold(10.0)).color(WARN));
                    }
                });
                let mut sub = if m.is_self {
                    "This PC".to_string()
                } else if !m.connected {
                    "Connecting…".to_string()
                } else if !m.via.is_empty() {
                    format!("{} ms through {}", m.rtt.as_millis().max(1), m.via)
                } else {
                    format!("{} ms", m.rtt.as_millis().max(1))
                };
                if let Some(p) = pv {
                    if p.dist >= 0.0 {
                        sub += &format!(" · {:.0} m", p.dist);
                    }
                }
                let pres = if m.is_self { Some((vs.in_garage, app.voice.sub())) } else { app.voice.peer_presence(m.id) };
                if let Some((garage, team)) = pres {
                    if garage {
                        sub += " · garage";
                    }
                    if !team.is_empty() {
                        sub += &format!(" · {team}");
                    }
                }
                ui.label(RichText::new(sub).size(12.5).color(if m.connected { muted() } else { WARN }));
            });
            if !m.is_self {
                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                    let open = *menu_for == Some(m.id);
                    let (r, resp) = ui.allocate_exact_size(vec2(28.0, 28.0), Sense::click());
                    if open || resp.hovered() {
                        ui.painter().rect_filled(r, 8.0, white(0x26));
                    }
                    for dy in [-6.0, 0.0, 6.0] {
                        ui.painter().circle_filled(pos2(r.center().x, r.center().y + dy), 1.8, muted());
                    }
                    if resp.on_hover_text("Volume and more").on_hover_cursor(egui::CursorIcon::PointingHand).clicked() {
                        *menu_for = if open { None } else { Some(m.id) };
                    }
                    if let Some(p) = pv {
                        ui.label(RichText::new(format!("{}%", p.volume)).size(12.5).color(muted()));
                        Self::bar(ui, p.volume as f32 / 100.0, if p.muffled > 30 { with_alpha(ACCENT, 0x90) } else { ACCENT }, 60.0);

                    }
                });
            }
        });
        if *menu_for == Some(m.id) && !m.is_self {
            egui::Frame::new().fill(black(0x28)).corner_radius(CornerRadius::same(14)).inner_margin(Margin::same(12)).show(ui, |ui| {
                ui.set_width(ui.available_width());
                let mut vol = app.settings.lock().unwrap().peer_volume.get(&m.name).copied().unwrap_or(100) as f32;
                if Self::slider(ui, &format!("Your volume for {}", m.name), &format!("{}%", vol as i32), &mut vol, 0.0..=200.0, 100.0) {
                    app.set_peer_volume(m.id, &m.name, vol as i32);
                    *save = true;
                }
                ui.horizontal_wrapped(|ui| {
                    if let Some(theirs) = app.voice.peer_settings(m.id) {
                        if Self::button(ui, "Copy their voice settings", Btn::Soft, false).clicked() {
                            app.voice.apply_settings(&theirs);
                            app.log(format!("Copied {}'s voice settings.", m.name));
                            *save = true;
                        }
                    }
                    if r.am_host() {
                        if Self::button(ui, if m.moderator { "Remove mod" } else { "Make mod" }, Btn::Soft, false).clicked() {
                            let _ = r.set_mod(m.id, !m.moderator);
                        }
                    }
                    if r.can_moderate() {
                        if !m.host && Self::button(ui, if m.muted { "Unmute for everyone" } else { "Mute for everyone" }, Btn::Danger, false).clicked() {
                            let _ = r.set_muted(m.id, !m.muted);
                        }
                        let iso = rules.isolate == m.id;
                        if Self::button(ui, if iso { "Stop isolating" } else { "Isolate for everyone" }, Btn::Soft, false).clicked() {
                            let _ = r.set_rules(RoomRules { isolate: if iso { 0 } else { m.id }, ..rules.clone() });
                        }
                    }
                });
                if r.can_moderate() {
                    Self::caption(ui, "Isolate: everyone hears only this person, everyone else faded. Mods can mute, unmute and isolate; only the host makes mods, and nobody can mute the host.");
                }
            });
        }
    }

    fn pits(ui: &mut Ui, app: &Arc<App>, r: &Arc<Room>, rules: &RoomRules, vs: &VoiceStatus, sub_new: &mut String) {
        ui.horizontal(|ui| {
            Self::section(ui, "Pits");
            if vs.in_garage {
                Self::pill(ui, "You're in the garage", ACCENT);
            }
        });
        let can = r.can_moderate();
        ui.horizontal(|ui| {
            ui.label(RichText::new("Pit radio").font(semi(14.5)));
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                if can {
                    if Self::switch(ui, rules.pit_radio) {
                        let _ = r.set_rules(RoomRules { pit_radio: !rules.pit_radio, ..rules.clone() });
                    }
                } else {
                    Self::pill(ui, if rules.pit_radio { "On" } else { "Off" }, if rules.pit_radio { GOOD } else { faint() });
                }
            });
        });
        Self::caption(
            ui,
            if rules.pit_radio {
                "On: in the garage everyone hears each other at full volume. Changes it for the whole room."
            } else {
                "Off: in the garage you hear people by distance. Changes it for the whole room."
            },
        );
        // sub-rooms: the room's permanent ones plus any that someone is in right now
        let mine = app.voice.sub();
        let mut subs: Vec<String> = rules.subs.clone();
        for s in app.voice.people_subs() {
            if !subs.iter().any(|x| x.eq_ignore_ascii_case(&s)) {
                subs.push(s);
            }
        }
        ui.label(RichText::new("Sub-room").font(semi(14.5)));
        ui.horizontal_wrapped(|ui| {
            let chip = |ui: &mut Ui, label: &str, sel: bool| -> bool {
                ui.add(egui::Button::new(RichText::new(label).font(semi(13.0)).color(if sel { ACCENT_D } else { TEXT })).fill(if sel { ACCENT } else { white(0x1c) }).corner_radius(CornerRadius::same(14)).min_size(vec2(0.0, 28.0)))
                    .clicked()
            };
            if chip(ui, "Everyone", mine.is_empty()) {
                app.voice.set_sub("");
            }
            for s in &subs {
                let sel = s.eq_ignore_ascii_case(&mine);
                if chip(ui, s, sel) {
                    app.voice.set_sub(if sel { "" } else { s });
                }
                let permanent = rules.subs.iter().any(|x| x.eq_ignore_ascii_case(s));
                if can && permanent && ui.small_button("x").on_hover_text("Remove this sub-room for everyone").clicked() {
                    let _ = r.set_rules(RoomRules { subs: rules.subs.iter().filter(|x| !x.eq_ignore_ascii_case(s)).cloned().collect(), ..rules.clone() });
                }
            }
        });
        ui.horizontal(|ui| {
            let w = ui.available_width() - if can { 210.0 } else { 100.0 };
            let resp = Self::text_field(ui, sub_new, "New sub-room, e.g. your team", false, w.max(120.0));
            let enter = resp.lost_focus() && ui.input(|i| i.key_pressed(egui::Key::Enter));
            let name: String = sub_new.trim().chars().take(24).collect();
            if (Self::button(ui, "Join", Btn::Soft, false).clicked() || enter) && !name.is_empty() {
                app.voice.set_sub(&name);
                sub_new.clear();
            }
            if can && Self::button(ui, "Keep for room", Btn::Soft, false).on_hover_text("Adds it as a permanent sub-room everyone can pick").clicked() && !name.is_empty() {
                let mut subs = rules.subs.clone();
                subs.push(name.clone());
                let _ = r.set_rules(RoomRules { subs, ..rules.clone() });
                sub_new.clear();
            }
        });
        Self::caption(ui, "In the garage with pit radio on you only hear and talk to your sub-room; on track you hear everyone as usual. Broadcast goes only to your sub-room if you're in one (team radio).");
    }

    fn radar_card(&mut self, ui: &mut Ui, members: &[MemberInfo], vs: &VoiceStatus) {
        let range = self.app.voice.range_m.load(Ordering::Relaxed) as f32;
        let have_pos = vs.have_pos;
        self.card(ui, Icon::Radar, "Radar", |ui| Self::pill(ui, &format!("{range:.0} m"), faint()), |ui| {
            let size = 230.0f32.min(ui.available_width());
            let (rect, _) = ui.allocate_exact_size(vec2(ui.available_width(), size), Sense::hover());
            let c = rect.center();
            let rad = size / 2.0 - 12.0;
            let p = ui.painter();
            p.circle_filled(c, rad, black(0x30));
            for f in [1.0, 0.55, 0.1] {
                p.circle_stroke(c, rad * f, Stroke::new(1.0_f32, white(if f == 1.0 { 0x30 } else { 0x16 })));
            }
            p.line_segment([pos2(c.x, c.y - rad), pos2(c.x, c.y + rad)], Stroke::new(1.0_f32, white(0x0c)));
            p.line_segment([pos2(c.x - rad, c.y), pos2(c.x + rad, c.y)], Stroke::new(1.0_f32, white(0x0c)));
            // you: a little arrow pointing the way your car is going
            let you = [pos2(c.x, c.y - 9.0), pos2(c.x - 6.0, c.y + 6.0), pos2(c.x, c.y + 3.0), pos2(c.x + 6.0, c.y + 6.0)];
            p.add(egui::Shape::convex_polygon(you.to_vec(), if have_pos { TEXT } else { faint() }, Stroke::NONE));
            let mut off_track = Vec::new();
            for m in members.iter().filter(|m| !m.is_self) {
                let Some(pv) = vs.peers.get(&m.id) else { continue };
                let Some(rel) = pv.rel else {
                    off_track.push(m.name.as_str());
                    continue;
                };
                let d = (rel[0] * rel[0] + rel[1] * rel[1]).sqrt().max(0.001);
                // beyond the range they sit on the rim, faded
                let k = (d / range).min(1.0) * rad / d;
                let at = pos2(c.x + rel[0] * k, c.y - rel[1] * k);
                let col = avatar_color(&m.name);
                let fade = if d > range { 0x60 } else { 0xff };
                if pv.talking {
                    p.circle_stroke(at, 11.0, Stroke::new(2.0_f32, with_alpha(GOOD, fade)));
                }
                p.circle_filled(at, 7.0, with_alpha(col, fade));
                p.text(pos2(at.x, at.y - 16.0), Align2::CENTER_CENTER, &m.name, semi(11.5), with_alpha(TEXT, fade));
            }
            p.text(pos2(c.x, c.y - rad + 10.0), Align2::CENTER_CENTER, "ahead", FontId::proportional(10.5), faint());
            if !have_pos {
                p.text(pos2(c.x, c.y + 28.0), Align2::CENTER_CENTER, "Shows who's around you once you're driving", FontId::proportional(12.5), muted());
            } else if !off_track.is_empty() {
                p.text(pos2(c.x, rect.bottom() - 4.0), Align2::CENTER_BOTTOM, format!("Not on track: {}", off_track.join(", ")), FontId::proportional(11.5), faint());
            }
        });
    }

    fn bind_row(ui: &mut Ui, app: &Arc<App>, act: &'static str, label: &str) {
        let cur = app.bind.bind(act);
        let capturing = app.bind.capturing_for().as_deref() == Some(act);
        ui.horizontal(|ui| {
            ui.label(RichText::new(label).font(semi(14.5)));
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                if !cur.is_empty() && Self::button(ui, "Clear", Btn::Ghost, false).clicked() {
                    app.clear_bind(act);
                }
                let text = if capturing { "Press a key or button…".to_string() } else { bind_name(&cur) };
                if Self::button(ui, &text, if capturing { Btn::Primary } else { Btn::Soft }, false).clicked() {
                    if capturing {
                        app.bind.cancel_capture();
                    } else {
                        app.bind.capture(act);
                    }
                }
            });
        });
    }

    fn device_picker(ui: &mut Ui, id: &str, icon: Icon, title: &str, current: &str, list: &[AudioDevice]) -> Option<String> {
        let mut out = None;
        let shown = if current.is_empty() { "System default".to_string() } else { list.iter().find(|d| d.id == current).map(|d| d.name.clone()).unwrap_or_else(|| short_device(current)) };
        ui.horizontal(|ui| {
            draw_icon(ui, icon, ACCENT, 16.0);
            ui.label(RichText::new(title).color(muted()));
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                egui::ComboBox::from_id_salt(id).selected_text(RichText::new(ellipsis(&shown, 34)).font(semi(14.0))).width(260.0).show_ui(ui, |ui| {
                    if ui.selectable_label(current.is_empty(), "System default").clicked() {
                        out = Some(String::new());
                    }
                    for d in list {
                        if ui.selectable_label(d.id == current, ellipsis(&d.name, 48)).clicked() {
                            out = Some(d.id.clone());
                        }
                    }
                });
            });
        });
        out
    }

    fn voice_card(&mut self, ui: &mut Ui, vs: &VoiceStatus, in_room: bool) {
        let app = self.app.clone();
        let v = app.voice.clone();
        if self.dev_at.map(|t| t.elapsed() > Duration::from_secs(5)).unwrap_or(true) {
            self.dev_at = Some(Instant::now());
            let fake = std::env::var("RINGLINK_FAKE_AUDIO").map(|x| x == "1").unwrap_or(false);
            self.mics = if fake { audio::fake::list_mics() } else { audio::list_mics() };
            self.spks = if fake { audio::fake::list_speakers() } else { audio::list_speakers() };
        }
        let (mic_dev, spk_dev, wanted, fx_open) = {
            let s = app.settings.lock().unwrap();
            (s.mic_device.clone(), s.speaker_device.clone(), s.voice, s.fx_open)
        };
        let (mics, spks) = (self.mics.clone(), self.spks.clone());
        let mut dirty = false;
        let kb = app.bind.keyboard_ok.load(Ordering::Relaxed);
        self.card(
            ui,
            Icon::Mic,
            "Voice",
            |ui| {
                if !vs.err.is_empty() {
                    Self::pill(ui, "Problem", BAD);
                } else if vs.on {
                    Self::pill(ui, "On", GOOD);
                } else {
                    Self::pill(ui, if wanted { "Starts in a room" } else { "Off" }, faint());
                }
            },
            |ui| {
                if !vs.err.is_empty() {
                    ui.label(RichText::new(capitalize(&vs.err)).size(13.0).color(BAD));
                }
                Self::section(ui, "Devices");
                if let Some(d) = Self::device_picker(ui, "mic", Icon::Mic, "Microphone", &mic_dev, &mics) {
                    app.set_devices(&d, &spk_dev);
                }
                if let Some(d) = Self::device_picker(ui, "spk", Icon::Phones, "Output", &spk_dev, &spks) {
                    app.set_devices(&mic_dev, &d);
                }
                ui.separator();
                ui.columns(3, |c| {
                    let m = v.muted.load(Ordering::Relaxed);
                    if Self::button(&mut c[0], if m { "Unmute mic" } else { "Mute mic" }, if m { Btn::Danger } else { Btn::Soft }, true).clicked() {
                        v.muted.store(!m, Ordering::Relaxed);
                    }
                    let d = v.deafened.load(Ordering::Relaxed);
                    if Self::button(&mut c[1], if d { "Undeafen" } else { "Deafen" }, if d { Btn::Danger } else { Btn::Soft }, true).on_hover_text("Stop hearing everyone").clicked() {
                        v.deafened.store(!d, Ordering::Relaxed);
                    }
                    if Self::button(&mut c[2], if wanted { "Voice off" } else { "Voice on" }, Btn::Ghost, true).clicked() {
                        app.settings.lock().unwrap().voice = !wanted;
                        if in_room {
                            app.set_voice(!wanted);
                        } else {
                            dirty = true;
                        }
                    }
                });
                let bc = v.bcast_btn.load(Ordering::Relaxed);
                if Self::button(ui, if bc { "Broadcasting to everyone - click to stop" } else { "Broadcast to everyone" }, if bc { Btn::Primary } else { Btn::Soft }, true).clicked() {
                    v.bcast_btn.store(!bc, Ordering::Relaxed);
                    v.kick_pos();
                }
                Self::bind_row(ui, &app, ACT_BROADCAST, "Push-to-broadcast");
                Self::caption(
                    ui,
                    if kb {
                        "Hold it and everyone (or just your sub-room, if you picked one) hears you at full volume, wherever they are. Keys and wheel / button-box buttons work while the game is focused."
                    } else {
                        "Hold it and everyone (or just your sub-room, if you picked one) hears you at full volume, wherever they are. Use a button on your wheel / button box (keyboard keys need an X11 desktop)."
                    },
                );
                ui.separator();
                Self::section(ui, "How your mic sends");
                let ptt = v.ptt_mode.load(Ordering::Relaxed);
                if let Some(i) = Self::segmented(ui, &["Voice activated", "Push to talk"], ptt as usize) {
                    v.ptt_mode.store(i == 1, Ordering::Relaxed);
                    dirty = true;
                }
                if ptt {
                    Self::bind_row(ui, &app, ACT_PTT, "Push to talk");
                }
                Self::bind_row(ui, &app, ACT_PTM, "Push to mute");
                Self::bind_row(ui, &app, ACT_MUTE, "Mute on / off");
                ui.horizontal(|ui| {
                    ui.label(RichText::new("Mic level").font(semi(14.5)));
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        let (txt, c) = if !vs.on {
                            ("Off", faint())
                        } else if v.muted.load(Ordering::Relaxed) || v.host_mute.load(Ordering::Relaxed) {
                            ("Muted", BAD)
                        } else if vs.talking {
                            ("Sending", GOOD)
                        } else {
                            ("Quiet", faint())
                        };
                        ui.add_sized(vec2(52.0, 16.0), egui::Label::new(RichText::new(txt).size(12.5).color(c)));
                        let w = ui.available_width();
                        Self::bar(ui, vs.level as f32 / 100.0, if vs.talking { GOOD } else { white(0x70) }, w);
                    });
                });
                if !ptt {
                    dirty |= Self::slider_atomic(ui, "Mic sensitivity", &v.sens.load(Ordering::Relaxed).to_string(), &v.sens, 0.0..=100.0, 60.0);
                    Self::caption(ui, "Raise it if your voice cuts out, lower it if background noise gets through.");
                }
                dirty |= Self::switch_atomic(ui, "Noise suppression", "Takes wheel motor whine, fans and keyboard clatter out of your mic.", &v.denoise);
                Self::switch_atomic(ui, "Hear myself (mic test)", "Plays your own mic back to you so you can check how you sound. Others still hear you normally.", &v.mic_test);
                ui.separator();
                ui.horizontal(|ui| {
                    let r = ui.add(egui::Label::new(RichText::new(if fx_open { "OTHER PEOPLE'S VOICES  -" } else { "OTHER PEOPLE'S VOICES  +" }).font(semi(11.5)).color(faint())).sense(Sense::click()));
                    if r.on_hover_cursor(egui::CursorIcon::PointingHand).clicked() {
                        app.settings.lock().unwrap().fx_open = !fx_open;
                        dirty = true;
                    }
                    if fx_open {
                        ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                            if Self::button(ui, "Reset", Btn::Ghost, false).clicked() {
                                v.apply_settings(&VoiceSettings::default());
                                dirty = true;
                            }
                        });
                    }
                });
                if fx_open {
                    let rng = v.range_m.load(Ordering::Relaxed);
                    dirty |= Self::slider_atomic(ui, "Volume", &format!("{}%", v.vol_pct.load(Ordering::Relaxed)), &v.vol_pct, 0.0..=200.0, 100.0);
                    dirty |= Self::slider_atomic(ui, "How far voices carry", &format!("{rng} m"), &v.range_m, 20.0..=500.0, 150.0);
                    Self::caption(ui, &format!("Full volume within {} m, silent at {rng} m.", rng / 10));
                    let mf = v.muffle.load(Ordering::Relaxed);
                    dirty |= Self::slider_atomic(ui, "Muffle with distance", &format!("{mf}%"), &v.muffle, 0.0..=100.0, 50.0);
                    Self::caption(ui, muffle_desc(mf));
                    dirty |= Self::slider_atomic(ui, "Always muffled", &format!("{}%", v.base_muff.load(Ordering::Relaxed)), &v.base_muff, 0.0..=100.0, 0.0);
                    Self::caption(ui, "Muffles everyone all the time, like talking through helmets or a team radio.");
                    dirty |= Self::switch_atomic(ui, "Directional sound", "Voices come from where their car is: left, right or behind you. Uses your car's direction (head turning in VR isn't tracked).", &v.direct);
                    let dp = v.doppler.load(Ordering::Relaxed);
                    dirty |= Self::slider_atomic(ui, "Doppler effect", &format!("{dp}%"), &v.doppler, 0.0..=100.0, 50.0);
                    Self::caption(ui, doppler_desc(dp));
                }
                ui.separator();
                dirty |= Self::switch_atomic(ui, "Sounds", "A short tone when someone joins, leaves or starts a broadcast.", &v.sounds);
                let rp = v.replay.enabled();
                if Self::switch_row(ui, "Voice in replays", &replay_caption(&v.replay.status(), rp), rp) {
                    v.replay.set_enabled(!rp);
                    dirty = true;
                }
                if let Some(what) = v.replay.playing() {
                    egui::Frame::new().fill(with_alpha(ACCENT, 0x28)).corner_radius(CornerRadius::same(12)).inner_margin(Margin::symmetric(12, 8)).show(ui, |ui| {
                        ui.set_width(ui.available_width());
                        ui.horizontal(|ui| {
                            ui.label(RichText::new(format!("Playing voice saved {what}")).size(13.0).color(ACCENT));
                            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                if ui.link(RichText::new("Wrong session").font(semi(13.0)).color(WARN)).on_hover_text("Stops it and never plays this recording with a replay again").clicked() {
                                    v.replay.reject_playing();
                                }
                            });
                        });
                    });
                }
                dirty |= Self::switch_atomic(ui, "Keep my voice out of replays", "Other people's RingLink won't save your voice, and yours won't either.", &v.no_record);
                Self::caption(ui, "Use headphones so nobody hears themselves echo.");
                if !vs.backend.is_empty() && vs.on {
                    Self::caption(ui, &format!("Audio: {}", vs.backend));
                }
            },
        );
        if dirty {
            self.touch();
        }
    }

    fn look_card(&mut self, ui: &mut Ui) {
        let app = self.app.clone();
        let (mut op, mut pa, top, radar) = {
            let s = app.settings.lock().unwrap();
            (s.opacity as f32, s.panel_opacity as f32, s.always_on_top, s.show_radar)
        };
        let mut dirty = false;
        self.card(ui, Icon::Look, "Appearance", |_| {}, |ui| {
            if Self::slider(ui, "Background", &format!("{}%", op as i32), &mut op, 20.0..=100.0, 100.0) {
                app.settings.lock().unwrap().opacity = op as i32;
                dirty = true;
            }
            if op < 100.0 {
                if Self::slider(ui, "Panels & buttons", &format!("{}%", pa as i32), &mut pa, 20.0..=100.0, 90.0) {
                    app.settings.lock().unwrap().panel_opacity = pa as i32;
                    dirty = true;
                }
            }
            Self::caption(ui, "Below 100% the window background goes see-through, so RingLink can sit over the game or your desktop.");
            if Self::switch_row(ui, "Always on top", "Keeps RingLink above other windows.", top) {
                app.settings.lock().unwrap().always_on_top = !top;
                dirty = true;
            }
            if Self::switch_row(ui, "Show radar", "A small map of who's around your car.", radar) {
                app.settings.lock().unwrap().show_radar = !radar;
                dirty = true;
            }
        });
        if dirty {
            self.touch();
        }
    }

    fn log_card(&mut self, ui: &mut Ui, room: Option<&Arc<Room>>) {
        let logs = self.app.logs();
        let traffic = room.map(|r| r.traffic());
        self.card(
            ui,
            Icon::List,
            "Activity",
            |ui| {
                if let Some((i, o)) = traffic {
                    ui.label(RichText::new(format!("↓ {}   ↑ {}", bytes(i), bytes(o))).size(12.0).color(faint()));
                }
            },
            |ui| {
                egui::ScrollArea::vertical().id_salt("log").max_height(130.0).stick_to_bottom(true).show(ui, |ui| {
                    ui.set_width(ui.available_width());
                    if logs.is_empty() {
                        Self::caption(ui, "Nothing yet.");
                    }
                    for l in logs.iter().rev().take(80).rev() {
                        ui.label(RichText::new(l).size(12.5).color(muted()));
                    }
                });
            },
        );
    }
}

impl eframe::App for RingLinkUi {
    fn clear_color(&self, _: &egui::Visuals) -> [f32; 4] {
        [0.0, 0.0, 0.0, 0.0]
    }

    fn update(&mut self, ctx: &egui::Context, _: &mut eframe::Frame) {
        let room = self.app.room();
        let vs = self.app.voice.status();
        let top = self.app.settings.lock().unwrap().always_on_top;
        if self.applied_top != Some(top) {
            self.applied_top = Some(top);
            ctx.send_viewport_cmd(egui::ViewportCommand::WindowLevel(if top { egui::WindowLevel::AlwaysOnTop } else { egui::WindowLevel::Normal }));
        }
        let a = self.opacity().0;
        egui::CentralPanel::default().frame(egui::Frame::new()).show(ctx, |ui| {
            // backdrop: a soft diagonal gradient
            let r = ui.max_rect();
            let al = (a * 255.0) as u8;
            let mut mesh = egui::Mesh::default();
            mesh.colored_vertex(r.left_top(), alpha(BG2, al));
            mesh.colored_vertex(r.right_top(), alpha(BG, al));
            mesh.colored_vertex(r.right_bottom(), alpha(BG2, al));
            mesh.colored_vertex(r.left_bottom(), alpha(BG, al));
            mesh.add_triangle(0, 1, 2);
            mesh.add_triangle(0, 2, 3);
            ui.painter().add(egui::Shape::mesh(mesh));
            egui::ScrollArea::vertical().auto_shrink([false, false]).show(ui, |ui| {
                let w = ui.available_width().min(600.0);
                let pad = ((ui.available_width() - w) / 2.0).max(0.0) + 16.0;
                egui::Frame::new().inner_margin(Margin { left: pad as i8, right: pad as i8, top: 16, bottom: 16 }).show(ui, |ui| {
                    self.header(ui, room.as_ref());
                    self.game_card(ui, &vs, room.is_some());
                    match &room {
                        Some(r) => {
                            self.in_room(ui, r, &vs);
                            if self.app.settings.lock().unwrap().show_radar {
                                let members = r.members();
                                self.radar_card(ui, &members, &vs);
                            }
                        }
                        None => self.lobby(ui),
                    }
                    self.voice_card(ui, &vs, room.is_some());
                    self.look_card(ui);
                    self.log_card(ui, room.as_ref());
                    ui.vertical_centered(|ui| ui.label(RichText::new(format!("RingLink {APP_VERSION}")).size(11.5).color(faint())));
                });
            });
        });
        // settings are saved a moment after the last change, not on every slider step
        if self.dirty && self.last_save.elapsed() > Duration::from_millis(600) && !ctx.input(|i| i.pointer.any_down()) {
            self.dirty = false;
            self.last_save = Instant::now();
            self.app.save();
        }
        // meters and the radar move while you're in a room
        ctx.request_repaint_after(Duration::from_millis(if room.is_some() { 50 } else { 400 }));
    }

    fn on_exit(&mut self, _: Option<&eframe::glow::Context>) {
        self.app.save();
        self.app.shutdown();
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Icon {
    Car,
    People,
    Radar,
    Mic,
    Phones,
    Look,
    List,
}

/// Small line icons drawn by hand, so they never depend on which fonts are installed.
fn draw_icon(ui: &mut Ui, icon: Icon, c: Color32, size: f32) {
    let (r, _) = ui.allocate_exact_size(Vec2::splat(size), Sense::hover());
    let p = ui.painter();
    let u = size / 18.0;
    let o = r.min;
    let at = |x: f32, y: f32| pos2(o.x + x * u, o.y + y * u);
    let st = Stroke::new(1.6 * u, c);
    match icon {
        Icon::Car => {
            p.add(egui::Shape::convex_polygon(vec![at(2.0, 12.0), at(3.5, 8.0), at(6.0, 5.0), at(12.0, 5.0), at(14.5, 8.0), at(16.0, 12.0)], c, Stroke::NONE));
            p.rect_filled(Rect::from_min_max(at(2.0, 10.0), at(16.0, 13.5)), 1.5 * u, c);
            p.circle_filled(at(5.5, 14.0), 1.8 * u, c);
            p.circle_filled(at(12.5, 14.0), 1.8 * u, c);
        }
        Icon::People => {
            p.circle_filled(at(6.5, 6.0), 2.6 * u, c);
            p.circle_filled(at(12.8, 6.8), 2.1 * u, c);
            p.rect_filled(Rect::from_min_max(at(1.5, 10.5), at(11.5, 15.0)), 2.5 * u, c);
            p.rect_filled(Rect::from_min_max(at(12.3, 11.0), at(17.0, 15.0)), 2.0 * u, c);
        }
        Icon::Radar => {
            p.circle_stroke(at(9.0, 9.0), 7.0 * u, st);
            p.circle_stroke(at(9.0, 9.0), 3.2 * u, Stroke::new(1.2 * u, c));
            p.circle_filled(at(12.5, 5.5), 1.5 * u, c);
        }
        Icon::Mic => {
            p.rect_filled(Rect::from_min_max(at(6.5, 1.5), at(11.5, 10.5)), 2.5 * u, c);
            p.add(egui::Shape::line(vec![at(4.0, 8.5), at(4.6, 11.0), at(6.5, 13.0), at(9.0, 13.6), at(11.5, 13.0), at(13.4, 11.0), at(14.0, 8.5)], st));
            p.line_segment([at(9.0, 13.6), at(9.0, 16.5)], st);
        }
        Icon::Phones => {
            p.add(egui::Shape::line(vec![at(3.0, 11.0), at(3.3, 7.5), at(5.0, 4.6), at(9.0, 3.2), at(13.0, 4.6), at(14.7, 7.5), at(15.0, 11.0)], st));
            p.rect_filled(Rect::from_min_max(at(2.0, 10.0), at(5.5, 15.5)), 1.5 * u, c);
            p.rect_filled(Rect::from_min_max(at(12.5, 10.0), at(16.0, 15.5)), 1.5 * u, c);
        }
        Icon::Look => {
            p.circle_stroke(at(9.0, 9.0), 6.8 * u, st);
            p.add(egui::Shape::convex_polygon(vec![at(9.0, 2.2), at(13.8, 4.2), at(15.8, 9.0), at(13.8, 13.8), at(9.0, 15.8)], c, Stroke::NONE));
        }
        Icon::List => {
            for y in [5.0, 9.0, 13.0] {
                p.circle_filled(at(3.5, y), 1.2 * u, c);
                p.line_segment([at(6.5, y), at(15.5, y)], st);
            }
        }
    }
}

pub fn decode_png(bytes: &[u8]) -> Option<(u32, u32, Vec<u8>)> {
    let mut dec = png::Decoder::new(bytes);
    dec.set_transformations(png::Transformations::normalize_to_color8() | png::Transformations::ALPHA);
    let mut rd = dec.read_info().ok()?;
    let mut buf = vec![0; rd.output_buffer_size()];
    let info = rd.next_frame(&mut buf).ok()?;
    buf.truncate(info.buffer_size());
    let rgba = match info.color_type {
        png::ColorType::Rgba => buf,
        png::ColorType::GrayscaleAlpha => buf.chunks(2).flat_map(|c| [c[0], c[0], c[0], c[1]]).collect(),
        _ => return None,
    };
    Some((info.width, info.height, rgba))
}

fn lerp_c(a: Color32, b: Color32, t: f32) -> Color32 {
    let t = t.clamp(0.0, 1.0);
    let f = |x: u8, y: u8| (x as f32 + (y as f32 - x as f32) * t) as u8;
    Color32::from_rgb(f(a.r(), b.r()), f(a.g(), b.g()), f(a.b(), b.b()))
}

fn avatar_color(name: &str) -> Color32 {
    let h = name.bytes().fold(0u32, |h, b| h.wrapping_mul(31).wrapping_add(b as u32));
    AVATARS[(h % AVATARS.len() as u32) as usize]
}

fn initials(name: &str) -> String {
    let parts: Vec<&str> = name.split(|c: char| c.is_whitespace() || c == '-' || c == '_').filter(|s| !s.is_empty()).collect();
    let s: String = match parts.len() {
        0 => "?".into(),
        1 => parts[0].chars().take(2).collect(),
        _ => parts.iter().take(2).filter_map(|p| p.chars().next()).collect(),
    };
    s.to_uppercase()
}

fn capitalize(s: &str) -> String {
    let mut c = s.chars();
    match c.next() {
        Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
        None => String::new(),
    }
}

fn ellipsis(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        format!("{}…", s.chars().take(n - 1).collect::<String>())
    }
}

fn short_device(id: &str) -> String {
    ellipsis(id.rsplit('.').next().unwrap_or(id), 34)
}

fn bytes(n: i64) -> String {
    if n >= 1 << 20 {
        format!("{:.1} MB", n as f64 / (1 << 20) as f64)
    } else {
        format!("{:.0} kB", n as f64 / 1024.0)
    }
}

fn muffle_desc(m: i32) -> &'static str {
    match m {
        0 => "Off: far voices are quieter but stay clear.",
        1..=35 => "A little duller when far away.",
        36..=70 => "Like hearing them through a wall when far.",
        _ => "Far voices are heavily muffled.",
    }
}

fn doppler_desc(d: i32) -> &'static str {
    match d {
        0 => "Off: no pitch change.",
        1..=60 => "Voices pitch up a little when a car closes on you and down as it pulls away.",
        61..=99 => "Stronger pitch change, noticeable at big speed differences.",
        _ => "Realistic: the same shift a real sound source would have.",
    }
}

fn replay_caption(st: &(bool, bool), on: bool) -> String {
    if !on {
        return "Off: voice chat isn't saved.".into();
    }
    match st {
        (_, true) => "Watching a replay: playing back what was said at this moment.".into(),
        (true, _) => "Recording this session's voice chat for its replay.".into(),
        _ => "Saves voice chat while you drive in LMU and plays it back in sync when you watch the replay.".into(),
    }
}
