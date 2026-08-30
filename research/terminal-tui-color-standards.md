# Terminal TUI color standards and reference practices

**Research date:** 2026-08-30  
**Scope:** accessibility baselines, terminal color constraints, Lip Gloss behavior,
and first-party practices from OpenAI Codex CLI and Anthropic Claude Code.

## Executive conclusion

There is no single standards-defined terminal palette whose RGB values an
application can trust. The most portable baseline is terminal-default foreground
and background for primary text, a small set of semantic ANSI colors for accents,
and redundant text or glyph cues for every state. Custom RGB palettes are
reasonable only when foreground and background are controlled as a tested pair
with separate light/dark variants and explicit fallbacks.

For SelfMind, the lowest-risk direction is a hybrid of the options below:

- use the terminal's default foreground for narration, commands, targets, and
  other essential text;
- reserve color for markers, selection, success, warning, and error;
- never apply `Faint`/dim to text required to understand the current action;
- keep state understandable after all ANSI color is stripped;
- detect and test no-color, ANSI-16, ANSI-256, and truecolor separately.

## Sourced facts

### Accessibility baseline

WCAG is a Web standard, not a terminal conformance specification. It is still a
useful, technology-neutral measurement baseline for terminal UI design:

- WCAG 2.2 says color must not be the only visual means of conveying information,
  actions, prompts, or distinctions ([SC 1.4.1](https://www.w3.org/TR/WCAG22/#use-of-color)).
- Normal text requires at least `4.5:1` contrast and large text `3:1` at Level AA
  ([SC 1.4.3](https://www.w3.org/TR/WCAG22/#contrast-minimum)). Terminal text
  should normally be treated as normal text because applications do not control
  the user's font size.
- Visual information needed to identify a UI component or its state requires
  `3:1` contrast against adjacent colors
  ([SC 1.4.11](https://www.w3.org/TR/WCAG22/#non-text-contrast)). This is relevant
  to selection carets, focus borders, status bullets, and similar TUI symbols.

Inference: SelfMind should use these ratios as design gates for explicit RGB
foreground/background pairs, but should not claim WCAG conformance for an ANSI
color name whose displayed RGB value is controlled by the user's terminal.

### ANSI and terminal constraints

- ECMA-48 defines SGR attributes and semantic color slots: `30`–`37` for named
  foreground colors, `40`–`47` for named backgrounds, and `39`/`49` for
  implementation-defined defaults. It defines SGR `2` as faint/decreased
  intensity, not as a precise luminance or contrast value
  ([ECMA-48, section 8.3.117](https://ecma-international.org/wp-content/uploads/ECMA-48_5th_edition_june_1991.pdf)).
- `terminfo` exposes terminal capabilities such as maximum colors and color
  pairs, says a terminal is free to map portable color indices as it likes, and
  records that some terminals have collisions between color and attributes such
  as dim, bold, or underline
  ([ncurses `terminfo(5)`](https://invisible-island.net/ncurses/man/terminfo.5.html)).
- XTerm's 256-color model consists of the first 16 ANSI/system colors followed by
  a color cube and grayscale ramp. Palettes can be changed, including through
  control sequences; indexed and direct-color support are extensions layered on
  top of the base SGR model
  ([XTerm FAQ](https://invisible-island.net/xterm/xterm.faq.html#xterm_256_colors),
  [XTerm control sequences](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html)).

Practical consequence:

| Profile | What the app can request | What it cannot safely assume |
| --- | --- | --- |
| No color / ASCII | Text and structural glyphs | Any color distinction |
| ANSI-16 | Named semantic slots | Exact RGB or contrast of a named slot |
| ANSI-256 | An indexed palette | That indices 0–15 match a fixed RGB palette |
| Truecolor | An RGB foreground/background request | The user's default background, transparency, or display calibration |

`NO_COLOR` is an informal CLI convention: a non-empty `NO_COLOR` environment
variable asks software that colors output by default to suppress color. The
convention permits explicit per-application configuration or command-line flags
to override that default, and it does not require disabling bold or underline
([NO_COLOR](https://no-color.org/)).

### Lip Gloss behavior used by SelfMind

SelfMind currently uses Lip Gloss v1.1.0. That version supports ANSI-16,
ANSI-256, truecolor, and a one-bit ASCII profile; it automatically detects the
profile and coerces colors to the available gamut. It also provides adaptive
light/dark colors and complete per-profile colors
([Lip Gloss v1.1.0 README](https://github.com/charmbracelet/lipgloss/blob/v1.1.0/README.md#colors)).

Inference: automatic downsampling prevents unsupported escape sequences, but it
does not prove readable contrast. Quantizing a custom RGB foreground onto an
unknown user background can still make essential text unreadable.

### Mature coding CLI practices

OpenAI Codex CLI's checked-in TUI style guide is deliberately conservative:

- primary text uses the terminal default; secondary text uses dim;
- ANSI cyan is used for input tips, selection, and status; green for success;
  red for failure; and magenta for the Codex accent;
- custom colors and black/white foregrounds are discouraged because terminal
  themes vary; the guide also avoids blue and yellow
  ([Codex TUI style guide](https://github.com/openai/codex/blob/main/codex-rs/tui/styles.md)).

Codex's implementation separately classifies truecolor, ANSI-256, ANSI-16, and
unknown output. Arbitrary RGB colors are quantized only into the usually stable
portion of the 256-color table; ANSI-16/unknown falls back to the default color
([Codex `terminal_palette.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/terminal_palette.rs)).
Its diff renderer uses paired light/dark backgrounds only for richer color
profiles and falls back to foreground-only green/red in ANSI-16
([Codex `diff_render.rs`](https://github.com/openai/codex/blob/main/codex-rs/tui/src/diff_render.rs)).

Anthropic Claude Code takes a broader theme-system approach. Its official docs
provide automatic light/dark matching, light and dark variants, daltonized
themes, ANSI themes that use the terminal palette, and custom token themes with
RGB, ANSI-256, or ANSI values
([Claude Code terminal configuration](https://code.claude.com/docs/en/terminal-config#match-the-color-theme),
[Claude Code command reference](https://code.claude.com/docs/en/commands)).

These are two legitimate product choices, not contradictory standards: Codex
optimizes the default experience for portability; Claude Code spends more product
and testing complexity on user-selectable adaptation.

## SelfMind observation

SelfMind currently mixes a fixed dark RGB palette with ANSI indices. The fixed
palette is declared in
[`internal/ui/common/common.go`](../internal/ui/common/common.go), while action
verbs and state bullets use ANSI slots in
[`internal/gateway/cli/transcript_renderer.go`](../internal/gateway/cli/transcript_renderer.go).

Using the WCAG relative-luminance formula, the declared RGB pairs produce:

| Current token | On `PaletteBackground` `#2d001b` | On white terminal default | AA normal text |
| --- | ---: | ---: | --- |
| `PaletteText` `#f4edf2` | `16.18:1` | `1.15:1` | Pass only on the paired dark background |
| `PaletteMuted` `#a58c9d` | `6.07:1` | `3.07:1` | Pass only on the paired dark background |
| `PaletteSubtle` `#7e6676` | `3.59:1` | `5.19:1` | Fails normal text on the dark background |
| `PaletteBlue` `#2f9de8` | `6.31:1` | `2.95:1` | Pass only on the paired dark background |

This calculation does not say every current rendering fails: it shows that a
fixed foreground is safe only when its intended background is actually rendered.
Foreground-only uses of these tokens cannot assume the terminal is dark. The
existing text verbs, `›` marker, bullets, and Plan labels are valuable non-color
cues and should be retained.

## Comparison options for SelfMind

The following are recommendations/inference from the sourced constraints, not
requirements imposed by ECMA-48 or WCAG.

| Decision | A. ANSI-native semantic | B. Adaptive paired palette | C. User theme system |
| --- | --- | --- | --- |
| Primary text | Terminal default | Explicit light/dark foreground and background | Theme token |
| Accent/status | Small ANSI set | Tested RGB pairs, downsampled per profile | Theme token with ANSI fallback |
| Theme variability | Best default resilience | App owns dark/light pairing; transparency is harder | User selects auto/light/dark/ANSI/daltonized |
| Accessibility QA | Strip-color semantics + terminal matrix | Exact contrast tests per pair + terminal matrix | Same as B for every built-in theme |
| Brand control | Low | High | High and user-adjustable |
| Engineering cost | Low | Medium | High |
| Main risk | Bad user terminal palettes | Wrong background detection or incomplete pairing | Theme drift and much larger test surface |

### Recommended hybrid

Adopt A for the transcript and process mainline, with narrowly scoped B for
surfaces where SelfMind renders both foreground and background. Treat C as a
later product capability only if real users need theme choice.

Suggested semantic policy:

| Role | Recommended presentation | Required non-color cue |
| --- | --- | --- |
| Primary narration, command, path, target | Terminal-default foreground, normal intensity | Plain text itself |
| Current action | Default body; one cyan or magenta accent | `›` plus action wording |
| Running | Default or accent marker; avoid dimming essential text | `◦` and `Running`/present-tense verb |
| Success | ANSI green accent | `✓` or completed verb |
| Error/failure | ANSI red accent, optionally bold | `×`/`Error` and error text |
| Warning/approval risk | Tested amber only as accent | `Warning`/`Approval required` label |
| Selection/focus | Cyan accent or reverse style | `›`, brackets, or a border change |
| Secondary metadata | Dim only when omission would not change the decision | Label and layout remain understandable |
| Plan pending/done | Avoid faint for meaningful step text | `○` pending, `✓` done, `▶` active; strikethrough only supplementary |

The action type should be readable from its verb (`Read`, `Ran`, `Updated`) rather
than requiring separate read/run/write colors. This reduces the current rainbow
of ANSI cyan/magenta/yellow/blue and prevents a weak blue or yellow terminal slot
from hiding the mainline.

## Verification gate

Before accepting a palette change:

1. For every explicit foreground/background pair, calculate WCAG contrast:
   `>=4.5:1` for normal essential text and `>=3:1` for required state symbols.
2. Strip all ANSI sequences and confirm that action, selection, success, error,
   warning, and Plan state remain unambiguous.
3. Render golden frames for no-color, ANSI-16, ANSI-256, and truecolor. Include
   both light and dark terminal defaults; do not test only the development theme.
4. Manually inspect macOS Terminal/iTerm2, Windows Terminal through WSL, VS Code's
   integrated terminal, and tmux or SSH because capability reporting and palette
   forwarding differ.
5. Include narrow and wide terminals, Chinese/mixed-script narration, Approval,
   Plan, running tools, success, and failure. Essential content must never depend
   on `Faint`, italics, or color support.
6. Honor `NO_COLOR` by default and retain an explicit user override. Verify that
   the no-color path emits no color escapes while structural emphasis remains
   usable.

## Primary sources

- [W3C WCAG 2.2](https://www.w3.org/TR/WCAG22/)
- [ECMA-48, fifth edition](https://ecma-international.org/wp-content/uploads/ECMA-48_5th_edition_june_1991.pdf)
- [ncurses `terminfo(5)`](https://invisible-island.net/ncurses/man/terminfo.5.html)
- [XTerm control sequences](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html)
- [NO_COLOR convention](https://no-color.org/)
- [Lip Gloss v1.1.0](https://github.com/charmbracelet/lipgloss/blob/v1.1.0/README.md)
- [OpenAI Codex TUI sources](https://github.com/openai/codex/tree/main/codex-rs/tui)
- [Anthropic Claude Code terminal configuration](https://code.claude.com/docs/en/terminal-config)
