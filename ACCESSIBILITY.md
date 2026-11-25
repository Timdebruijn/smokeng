# Accessibility statement for smokeng

This statement covers the web interface that the smokeng binary serves. It follows the structure of the [W3C accessibility statement](https://www.w3.org/WAI/planning/statements/) and describes smokeng v0.30.0.

## Conformance status

smokeng is **partially conformant** with the [Web Content Accessibility Guidelines (WCAG) 2.2](https://www.w3.org/TR/WCAG22/), level AA. Partially conformant means that some parts of the content do not fully conform to the accessibility standard. Those parts are listed under [Known limitations](#known-limitations).

## How this was assessed

This is a self-evaluation. Nobody outside the project has audited smokeng.

In November 2026 the interface was audited against all 55 level A and AA criteria of WCAG 2.2: by reading the source, with automated checks in a browser, with scripted keyboard walks through every screen, and with measurements of contrast and layout in Chrome at 1280, 900 and 390 px, in the light and the dark theme. Most of what that found was fixed in v0.28.0, v0.28.1 and v0.29.0, and each fix was measured again in a running instance.

On 25 November 2026 the interface was audited again in the same way. That audit counted 16 of the 55 criteria as not met, most of them in one or two places. What it found that needs no visible change was fixed in v0.30.0 and measured again: that was everything it had listed under seven of the 16, and part of an eighth. Reading the code against this statement afterwards turned up more of the same kind under two of those seven, focus and announcements; that was fixed too, together with two of the visible findings (the contrast of the count on the bell, and revoking a token without confirmation). What is left, from all of this, is listed under [Known limitations](#known-limitations).

Not part of that assessment:

- a test with a real screen reader (NVDA, JAWS, VoiceOver) or with voice control;
- a test with people who rely on assistive technology;
- Safari and Firefox;
- the sign-in screen and the user menu, because the test instance ran without authentication;
- an alert that is firing on real packet loss.

The round that would cover most of this is written out in [Testing accessibility by hand](docs/accessibility-testing.md).

## What the interface provides

- Keyboard use: a skip link, one visible focus ring on every control, and plots whose cursor moves with the arrow keys. The forms for adding a target, a rule, a silence, an agent or a grant take the focus when they open and give it back when they close; a control that removes itself, such as a confirmed delete or "reset zoom", leaves the focus at the start of the screen; a header menu closes when the focus leaves it.
- What an action did is announced: acknowledging, silencing, saving, creating and deleting, every change of the time window on Graphs and on a target's detail, and the log scale.
- Structure a screen reader can use: a heading on every screen and on each target's plot on Graphs, lists for repeated rows, and a target tree whose rows say whether they are a group or a target and how deep they sit. Buttons in a row name the rule, agent, token, grant or target they act on.
- When the server refuses one of the forms and its message names one field, that field is marked and takes the focus.
- Plots are drawn on a canvas. Each has a text summary of its figures and a keyboard cursor that reads the interval under it, and the series behind each plot can be downloaded as a CSV file.
- Status dots have a shape besides their colour, and a text label for screen readers. The lines of a comparison have a dash pattern besides their colour.
- The `Aa` menu offers a higher-contrast palette, larger text and a colour-blind-safe palette. The interface follows the system's light or dark theme, reduced motion and forced colours.
- Screens that refresh themselves can be paused.
- Deleting a target, an alert rule, a silence, an agent or a grant, revoking an enrolment token and clearing a rule's captured reference ask for confirmation first.

## Known limitations

- **Explanations that only show on hover.** The "down" toggle on a target's detail and acknowledge in the bell menu explain themselves only in a tooltip, which keyboard and touch users cannot open. What acknowledge, silence, and capture and clear reference do is written on the Alerts screen, but only while something is firing or a rule that compares against a captured reference exists. (WCAG 3.3.2)
- **No data table behind a plot.** The series behind a plot can be downloaded as CSV and the keyboard cursor reads one interval at a time, but there is no table on the screen.
- **Lines of a comparison that look alike.** A comparison has eight pairs of colour and dash pattern. From the ninth line on they repeat, so two lines can look the same. With deuteranopia the third and fourth line (green and pink) come out close in lightness, so their dash pattern is what tells them apart; the colour-blind-safe palette does not change the comparison's lines. The keyboard cursor reads each line by name. (WCAG 1.4.1)
- **Contrast of the address in the selected tree row.** It is 4.2 to 1 in the light theme, just under the 4.5 asked for. (WCAG 1.4.3)
- **The selected row of the target tree** is shown by a faint tint: 1.2 to 1 against the rows beside it in the light theme, 1.4 to 1 in the dark. A screen reader is told which row is current. (WCAG 1.4.11 and 1.4.1)
- **Errors as the server wrote them.** A refused form shows the server's message unchanged. That can be database wording, such as "UNIQUE constraint failed" for a rule name that exists, and it does not say what to do. (WCAG 3.3.3) The fields and settings that are edited in place on Targets report a refusal at the top of the screen and not at the field. (WCAG 3.3.1)
- **"ack" in the bell menu, "acknowledge" on Alerts**, for the same action. (WCAG 3.2.4)
- **The format of a host** is given in the field's placeholder only, which disappears once you type. (WCAG 3.3.2)
- **Long screens for a keyboard without assistive technology.** The headings are something to jump to for a screen reader and for voice control. With the Tab key alone there are twenty stops on Graphs before the first plot on an instance with four targets, and more with more targets.
- **Where the focus lands after a control removes itself.** After a confirmed delete, a silence set from a firing row or "reset zoom", the focus goes to the start of the screen and not to the list or the toolbar the control was in.
- **Turning the comparison on or off** changes the plots without a message. (WCAG 4.1.3)
- **Wider text spacing on Targets.** With the text spacing of WCAG 1.4.12, the settings table makes the page scroll sideways at a window width of about 900 px. No text is lost.

## Compatibility

smokeng is tested in a current version of Chrome on a desktop. It relies on features that current versions of Edge, Firefox and Safari also have, among them the Popover API and the CSS `light-dark()` function, but it has not been tested there.

## Technologies

The interface relies on HTML, CSS, JavaScript and WAI-ARIA. JavaScript is required.

## Feedback

If something in smokeng does not work for you, open an issue at <https://github.com/Timdebruijn/smokeng/issues> and say which screen, which browser and which assistive technology. smokeng is maintained by one person in spare time; there is no promised response time.

## Date

This statement was written on 25 November 2026 for v0.30.0 and is reviewed with every release that changes the interface.
