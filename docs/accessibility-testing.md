# Testing accessibility by hand

The [accessibility statement](../ACCESSIBILITY.md) says what smokeng was measured against and
what was not covered. This page is the part a script cannot do: seven tasks to walk through
with the tools people actually use. One round takes an hour or two.

Use an instance with a few targets and an hour of data. A second vantage point, an alert rule
and a firing alert make tasks 2, 5 and 6 worth more; say in your notes when you did not have
them.

## What to test with

| Round | Setup | What it shows |
| --- | --- | --- |
| Keyboard | Any browser, no mouse | Everything can be reached and operated, and the focus is always visible |
| Screen reader | VoiceOver with Safari on macOS, or NVDA with Chrome or Firefox on Windows | Names, roles, states, announcements, the text behind the plots |
| Zoom | Browser zoom at 400% in a 1280 px wide window | Nothing is cut off and nothing scrolls sideways except data tables |
| Text spacing | A text-spacing bookmarklet or stylesheet (line height 1.5, letter spacing 0.12em, word spacing 0.16em) | No text is cut off or overlaps |
| Colours | The system's high-contrast or forced-colours mode, and the three options under `Aa` | Status and focus stay visible without the default colours |
| Phone | A phone, or 390 px wide in the browser's device mode | Everything is in view and large enough to press |

Do the keyboard round first. A screen reader round on top of a broken keyboard path mostly
finds the same problem again.

## The seven tasks

For each task: do it, then write down where you got stuck, what was not announced or not
visible, and what you had to guess. The expectations say what should happen. Several of them,
notably in tasks 1, 4 and 7, have never been checked with a screen reader, so a miss there is
a finding and not a mistake in your round.

1. **Find your way.** Open smokeng. Use the skip link. Go to every tab and back to Overview.
   Open and close the `Aa` menu and the bell.
   *Expect:* the page title and a heading name the screen you are on; the current tab is
   announced as current; focus starts at the top of the new screen; each menu closes with Esc
   and gives the focus back.
2. **Read a plot.** On Graphs, change the range, pause and resume live updates, switch the
   log scale. Tab to a plot and move its cursor with the arrow keys, Home and End. Turn on
   compare and read the comparison. Open the explanation of a quality badge if one shows.
   *Expect:* each plot has a sentence with its figures; the cursor reads one interval; a gap
   reads as no data, not as zero; pausing and resuming are announced.
3. **Prove a problem on one target.** Open a target's detail. Read its figures, the
   explanations behind Spread, Availability and Coverage, and the availability table. Export
   what can be exported.
   *Expect:* every figure has a label; the table has headers; an explanation opens with Enter
   and closes with Esc.
4. **Change the target tree.** On Targets, select a node, change a setting and revert it.
   Add a child with an empty name, then with a name that exists, then properly. Delete it.
   Open the TOML import and leave it again.
   *Expect:* the browser points at the empty required field; the server's refusal appears in
   the form and is read out; deleting asks for confirmation and Cancel puts the focus back;
   the dialog keeps the focus inside and returns it when closed.
5. **Set up alerting.** On Alerts, add a rule with a threshold that cannot be right (a loss
   of 150%), then a valid one. Schedule a silence and cancel it. If something is firing,
   acknowledge it.
   *Expect:* the wrong value is refused with the field named; a new firing alert is announced
   without moving the focus; a rule's state is given in words, not only as a coloured dot.
6. **Manage agents and access.** On Agents, issue a token, copy the command, revoke the
   token. On Access, add a grant and remove it.
   *Expect:* "Copied" is announced; the one-time token is reachable and readable; removing
   asks for confirmation.
7. **Jump and sign in.** Open the command palette with Cmd K or Ctrl K, find a target, go to
   it. If the instance has authentication, sign out and in again.
   *Expect:* the palette's field shows its focus; results are announced as you type; after
   signing in you land where you were going.

## Writing it down

Open an issue per barrier at <https://github.com/Timdebruijn/smokeng/issues> with the task
number, the screen, the browser and assistive technology with their versions, what you
expected and what happened. A barrier that stops a task outright goes into the known
limitations of the [accessibility statement](../ACCESSIBILITY.md) until it is fixed.

A round by someone who built the interface finds what is broken. It does not show what is
hard: for that, ask people who use a screen reader, magnification or voice control every day
to do the same seven tasks, and watch without helping.
