# Hyperframes Composition Brief: Lux

## Objective
A short, polished launch video for Lux, with narration, music and sound effects.

## Output
- Composition directory: `composition/`
- Rendered video: `brag.mp4`
- Format: landscape — 1920x1080, 30 fps
- Duration: 23.4 s

## Source Material
- Project root: `../` (lux)
- Primary files read: `desktop/src/renderer/styles.css`, `components/Sidebar.tsx`, `Menu.tsx`, `Activity.tsx`, `views/ChatView.tsx`, `ConnectorsView.tsx`, `Mascot.tsx`, `README.md`
- Product name: Lux
- Tagline / strongest claim: "A personal AI agent for everyone at your company." (README); always on, asks before it acts, self-hosted
- Key UI moments recreated with the app's own stylesheet: sidebar, + dropdown, agent chat with answer buttons, self-naming, Allow / Don't allow card, Connectors dropdown
- Copy that must appear verbatim: "Create an agent", "Allow", "Don't allow", "Connectors", "Accounts your agents can use for you. Agents never see the keys.", "Google", "Microsoft 365", "Slack", "Needs you", "Working…"

## Creative Direction
See `brag-plan.md` (tone: polished; hook, storyboard, outro and narration there).

## Visual Identity
- Background #101010, text #eee7cf, accent #D5360C / #f26a43, wordmark #E6D58D
- Fonts: Instrument Sans Variable, IBM Plex Mono (local files in `assets/fonts/`)
- GitClaw mascots in `assets/mascots/`

## Audio
- Music `assets/music/happy-beats-business-moves-vol-11-by-ende-dot-app.mp3`, volume automation ducks to 0.13 under narration
- Narration `assets/vo/vo1.wav` … `vo6.wav` (Kokoro af_heart), track 3
- SFX in `assets/sfx/` (Kenney CC0), chosen from `sfx-analysis.md` low/medium high-frequency-risk picks
- Beat locks: 1.60 s (hook line 2), 3.70 s (wordmark); beat grid 17.91 / 18.44 / 18.96 s (connectors)
- Audio-reactive: background glow scaled by per-frame music energy (`assets/music-energy.json`, extracted with ffmpeg + numpy because the hyperframes-creative helper is not installed here)

## Implementation note
Every frame is a pure function of time: a paused GSAP timeline tweens a clock whose setter calls `render(t)`, so any seek renders the same frame. CSS animations and transitions are disabled.
