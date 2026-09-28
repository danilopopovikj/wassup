# Keys

Press `?` in the TUI for this list.

| Key | Does |
| --- | --- |
| `←` `↑` `→` `↓` or `h` `j` `k` `l` | move the selection |
| `H` `J` `K` `L` or shift + arrows | pan the diagram |
| `enter` | open the detail panel |
| `tab` | toggle the panel |
| `+` `-` | resize the panel split |
| `i` | issue lens |
| `n` | next issue |
| `t` | scrub the timeline |
| `[` `]` | step 1 minute |
| `{` `}` | step 10 minutes |
| `esc` | back to live |
| `/` | filter by name |
| `d` | detail level: minimal, normal, full |
| `g` | collapse or expand the selected group |
| `f` | findings |
| `e` | changes |
| `a` | annotations |
| `x` `X` | clear the selected annotation, or all of them |
| `c` | copy the `wassup://` ref |
| `C` | copy the ref with a summary |
| `y` | copy the panel text |
| `s` | export svg, png and txt |
| `r` `R` | reset the layout of the selection, or of everything |
| `q` | quit |

## Mouse

| Action | Does |
| --- | --- |
| click | selects |
| drag a box | moves it |
| drag the bottom-right corner | resizes it |
| click a group title | collapses the group |
| click the timeline strip | jumps the scrub cursor |

Positions are saved to `.wassup/layout.json`. Boxes never move on their own.

## States

Every box and every edge is always in exactly one of six states.

| Glyph | State | Means |
| --- | --- | --- |
| `●` | flowing | work is moving |
| `○` | idle | bound, healthy, nothing happening |
| `≡` | waiting | work is queued at the destination |
| `◐` | processing | a long unit of work is running |
| `⊘` | blocked | traffic cannot pass |
| `✕` | failing | errors or crashes |

Two more markers describe wassup itself, not your system:

| Glyph | Marker | Means |
| --- | --- | --- |
| `┄` | unbound | no probe returned data |
| `◷` | stale | data older than 3 ticks |
