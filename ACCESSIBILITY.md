# Accessibility statement for smokeng

This statement covers the web interface that the smokeng binary serves. It follows the structure of the [W3C accessibility statement](https://www.w3.org/WAI/planning/statements/) and was last reviewed on 25 November 2026, against the main branch after v0.28.1.

## Conformance status

smokeng is **partially conformant** with the [Web Content Accessibility Guidelines (WCAG) 2.2](https://www.w3.org/TR/WCAG22/), level AA. Partially conformant means that some parts of the content do not fully conform to the accessibility standard. Those parts are listed under [Known limitations](#known-limitations).

## How this was assessed

This is a self-evaluation. Nobody outside the project has audited smokeng.

In November 2026 the interface was audited against all 55 level A and AA criteria of WCAG 2.2: by reading the source, with automated checks in a browser, with scripted keyboard walks through every screen, and with measurements of contrast and layout in Chrome at 1280, 900 and 390 px, in the light and the dark theme. Most of what that found was fixed in v0.28.0, in v0.28.1 and on the main branch since, and each fix was measured again in a running instance. What is left is listed under [Known limitations](#known-limitations).

Not part of that assessment:

- a test with a real screen reader (NVDA, JAWS, VoiceOver) or with voice control;
- a test with people who rely on assistive technology;
- Safari and Firefox;
- the sign-in screen and the user menu, because the test instance ran without authentication;
- an alert that is firing on real packet loss.

## What the interface provides

- Keyboard use: a skip link, one visible focus ring on every control, and plots whose cursor moves with the arrow keys.
- Plots are drawn on a canvas. Each has a text summary of its figures, and the keyboard cursor reads the interval under it.
- Status dots have a shape and a text label besides their colour, and the lines of a comparison each have their own dash pattern.
- The `Aa` menu offers a higher-contrast palette, larger text and a colour-blind-safe palette. The interface follows the system's light or dark theme, reduced motion and forced colours.
- Screens that refresh themselves can be paused.
- Deleting a target, an alert rule or a silence asks for confirmation first.

## Known limitations

- **Explanations that only show on hover.** The buttons custom, zoom out, compare, the "down" toggle, acknowledge, silence, and capture and clear reference explain themselves in a tooltip that keyboard and touch users cannot open. Their names are available to everyone; the extra explanation is not. (WCAG 3.3.2)
- **No data table behind a plot.** A plot has a text summary and a keyboard cursor, but the series itself cannot be read as a table or exported from the interface. The comparison plot has a summary per target and no keyboard cursor.
- **Two line colours in the comparison plot.** Orange and sky blue are below 3:1 against the light background. Each line also has its own dash pattern. (WCAG 1.4.11)
- **Wider text spacing on Targets.** With the text spacing of WCAG 1.4.12, the settings table makes the page scroll sideways at a window width of about 900 px. No text is lost.
- **Dense help text.** The explanations above the forms are long and use networking terms without introducing them.

## Compatibility

smokeng is tested in a current version of Chrome on a desktop. It relies on features that current versions of Edge, Firefox and Safari also have, among them the Popover API and the CSS `light-dark()` function, but it has not been tested there.

## Technologies

The interface relies on HTML, CSS, JavaScript and WAI-ARIA. JavaScript is required.

## Feedback

If something in smokeng does not work for you, open an issue at <https://github.com/Timdebruijn/smokeng/issues> and say which screen, which browser and which assistive technology. smokeng is maintained by one person in spare time; there is no promised response time.

## Date

This statement was written on 25 November 2026 and is reviewed with every release that changes the interface.
