#![cfg_attr(all(windows, not(debug_assertions)), windows_subsystem = "windows")]

use ringlink::app::{App, AppOptions, APP_VERSION};
use ringlink::demo::Demo;
use ringlink::ui::RingLinkUi;
use std::sync::{Arc, OnceLock};

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "--version" || a == "-V") {
        println!("RingLink {APP_VERSION}");
        return;
    }
    if args.iter().any(|a| a == "--help" || a == "-h") {
        println!("RingLink {APP_VERSION} - proximity voice chat for Le Mans Ultimate and BeamNG.drive\n\n  ringlink            open the app\n  ringlink --demo     try it with pretend drivers (no game, mic or friends needed)\n  ringlink --version");
        return;
    }
    let demo_mode = args.iter().any(|a| a == "--demo");

    // the app pokes the window whenever something changes
    let ctx_slot: Arc<OnceLock<egui::Context>> = Arc::new(OnceLock::new());
    let slot = ctx_slot.clone();
    let notify: Arc<dyn Fn() + Send + Sync> = Arc::new(move || {
        if let Some(c) = slot.get() {
            c.request_repaint();
        }
    });
    let (hub, opts) = if demo_mode {
        let dir = std::env::var("RINGLINK_CONFIG_DIR").map(Into::into).unwrap_or_else(|_| std::env::temp_dir().join(format!("ringlink-demo-{}", std::process::id())));
        let (h, o) = Demo::options(dir);
        (Some(h), o)
    } else {
        (None, AppOptions::default())
    };
    let app = App::new(notify, opts);
    let demo = hub.map(|h| Demo::start(h, &app));

    // the Night Stint tile icon, matching the other apps (the logo stays in the window)
    let icon = Some(egui::IconData { rgba: include_bytes!("../assets/icon-64.rgba").to_vec(), width: 64, height: 64 });
    let mut vp = egui::ViewportBuilder::default()
        .with_title("RingLink")
        .with_app_id("ringlink")
        .with_inner_size([560.0, 900.0])
        .with_min_inner_size([420.0, 480.0])
        .with_transparent(true);
    if let Some(i) = icon {
        vp = vp.with_icon(Arc::new(i));
    }
    let native = eframe::NativeOptions { viewport: vp, ..Default::default() };
    let a = app.clone();
    let res = eframe::run_native(
        "RingLink",
        native,
        Box::new(move |cc| {
            let _ = ctx_slot.set(cc.egui_ctx.clone());
            Ok(Box::new(RingLinkUi::new(cc, a)))
        }),
    );
    if let Some(d) = demo {
        d.stop();
    }
    app.shutdown();
    if let Err(e) = res {
        eprintln!("RingLink couldn't open its window: {e}");
        std::process::exit(1);
    }
}
