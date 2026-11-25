# Accessibility statement for smokeng

This statement covers the web interface that the smokeng binary serves. It follows the structure of the [W3C accessibility statement](https://www.w3.org/WAI/planning/statements/) and describes smokeng as it is on the main branch on 25 November 2026: v0.29.0 and what was merged after it.

## Conformance status

smokeng is **partially conformant** with the [Web Content Accessibility Guidelines (WCAG) 2.2](https://www.w3.org/TR/WCAG22/), level AA. Partially conformant means that some parts of the content do not fully conform to the accessibility standard. Those parts are listed under [Known limitations](#known-limitations).

## How this was assessed

This is a self-evaluation. Nobody outside the project has audited smokeng.

In November 2026 the interface was audited against all 55 level A and AA criteria of WCAG 2.2: by reading the source, with automated checks in a browser, with scripted keyboard walks through every screen, and with measurements of contrast and layout in Chrome at 1280, 900 and 390 px, in the light and the dark theme. Most of what that found was fixed in v0.28.0, v0.28.1, v0.29.0 and on the main branch since, and each fix was measured again in a running instance. What is left is listed under [Known limitations](#known-limitations).

Not part of that assessment:

- a test with a real screen reader (NVDA, JAWS, VoiceOver) or with voice control;
- a test with people who rely on assistive technology;
- Safari and Firefox;
- the sign-in screen and the user menu, because the test instance ran without authentication;
- an alert that is firing on real packet loss.

The round that would cover most of this is written out in [Testing accessibility by hand](docs/accessibility-testing.md).

## What the interface provides

- Keyboard use: a skip link, one visible focus ring on every control, and plots whose cursor moves with the arrow keys.
- Plots are drawn on a canvas. Each has a text summary of its figures and a keyboard cursor that reads the interval under it, and the series behind each plot can be downloaded as a CSV file.
- Status dots have a shape besides their colour, and a text label for screen readers. The lines of a comparison have a dash pattern besides their colour.
- The `Aa` menu offers a higher-contrast palette, larger text and a colour-blind-safe palette. The interface follows the system's light or dark theme, reduced motion and forced colours.
- Screens that refresh themselves can be paused.
- Deleting a target, an alert rule or a silence asks for confirmation first.

## Known limitations

- **Explanations that only show on hover.** The "down" toggle on a target's detail and acknowledge in the bell menu explain themselves only in a tooltip, which keyboard and touch users cannot open. What acknowledge, silence, and capture and clear reference do is written on the Alerts screen, but only while something is firing or a rule with a captured reference exists. (WCAG 3.3.2)
- **No data table behind a plot.** The series behind a plot can be downloaded as CSV and the keyboard cursor reads one interval at a time, but there is no table on the screen.
- **More than eight lines in a comparison.** A comparison has eight pairs of colour and dash pattern. From the ninth line on they repeat, so two lines can look the same. (WCAG 1.4.1)
- **Wider text spacing on Targets.** With the text spacing of WCAG 1.4.12, the settings table makes the page scroll sideways at a window width of about 900 px. No text is lost.

## Compatibility

smokeng is tested in a current version of Chrome on a desktop. It relies on features that current versions of Edge, Firefox and Safari also have, among them the Popover API and the CSS `light-dark()` function, but it has not been tested there.

## Technologies

The interface relies on HTML, CSS, JavaScript and WAI-ARIA. JavaScript is required.

## Feedback

If something in smokeng does not work for you, open an issue at <https://github.com/Timdebruijn/smokeng/issues> and say which screen, which browser and which assistive technology. smokeng is maintained by one person in spare time; there is no promised response time.

## Date

This statement was written on 25 November 2026 and is reviewed with every release that changes the interface.
