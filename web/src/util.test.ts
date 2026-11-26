import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { availabilityURL, fetchSeries, pathsURL, seriesURL, type Target } from './api.ts'
import { axisLabel } from './layout.ts'
import {
  graphGroups,
  graphGroupLabel,
  afterName,
  alertKey,
  alertName,
  RANGES,
  rangeWords,
  countedFlags,
  countUnacked,
  csvCell,
  firingAnnouncement,
  filterByFamily,
  flagCounts,
  flagNote,
  flagsIn,
  QUALITY_FLAGS,
  groupOfPath,
  groupRows,
  rowStatus,
  worstStatus,
  mayChange,
  parseFamilyChoice,
  resolveSeries,
  screenTitle,
  seriesKey,
  seriesLabel,
  shownFamilies,
  showsFamily,
  summarise,
  fromLocalInput,
  measurementLabel,
  minMax,
  refreshMs,
  toLocalInput,
  zoomOut,
} from './util.ts'

// A golden reference can hold hundreds of thousands of samples. Math.min(...xs)
// passes each element as an argument, and the engine refuses past about 125,000:
// a RangeError thrown while rendering, which blanked the whole page.
test('the spread form really does fail at the sizes a reference can reach', () => {
  const big = new Float64Array(1_000_000).fill(1)
  assert.throws(() => Math.min(...big), RangeError)
})

test('minMax handles any length', () => {
  const big = new Float64Array(1_000_000)
  for (let i = 0; i < big.length; i++) big[i] = (i * 7919) % 100_003
  big[500_000] = -4
  big[999_999] = 1_000_000
  assert.deepEqual(minMax(big), { lo: -4, hi: 1_000_000 })
  assert.deepEqual(minMax([3, 1, 2]), { lo: 1, hi: 3 })
  assert.deepEqual(minMax([5]), { lo: 5, hi: 5 })
  assert.deepEqual(minMax(new Uint32Array([9, 4, 11])), { lo: 4, hi: 11 })
})

test('minMax says so when there is nothing to take a minimum of', () => {
  assert.equal(minMax([]), null)
  // Not a number is not a value: it is skipped, where Math.min would turn the
  // whole answer into NaN.
  assert.deepEqual(minMax([3, NaN, 1]), { lo: 1, hi: 3 })
  assert.equal(minMax([NaN, NaN]), null)
  assert.deepEqual(minMax([-Infinity, 2, Infinity]), { lo: -Infinity, hi: Infinity })
})

// A target name is whatever an editor typed, and a spreadsheet reads a cell that
// starts with = + - or @ as a formula, which can run commands or send data out.
test('csvCell quotes text and neutralises what a spreadsheet would run', () => {
  assert.equal(csvCell('plain'), '"plain"')
  assert.equal(csvCell('say "hi"'), '"say ""hi"""')
  assert.equal(csvCell('a,b\nc'), '"a,b\nc"')
  for (const risky of ['=1+1', '+1', '-1', '@SUM(A1)', '\t=1', '\r=1', '=HYPERLINK("http://x","y")']) {
    const cell = csvCell(risky)
    assert.ok(cell.startsWith(`"'`), `${JSON.stringify(risky)} became ${cell}, which a spreadsheet would still read as a formula`)
    assert.ok(cell.includes(risky.replace(/"/g, '""')), 'the text itself is kept')
  }
  // Not at the start, nothing to neutralise.
  assert.equal(csvCell('a=1'), '"a=1"')
  assert.equal(csvCell(''), '""')
})

test('csvCell leaves numbers as numbers', () => {
  assert.equal(csvCell(0.9995), '0.9995')
  assert.equal(csvCell(-3.5), '-3.5')
  assert.equal(csvCell(0), '0')
  // No number to give is an empty cell, not the text "NaN" in a numeric column.
  assert.equal(csvCell(NaN), '')
  assert.equal(csvCell(Infinity), '')
})

test('a long window refreshes about once per pixel column, a short one every ten seconds', () => {
  assert.equal(refreshMs(3600), 10_000)
  assert.equal(refreshMs(86400), 86_400)
  assert.equal(refreshMs(30 * 86400), 2_592_000)
})

test('zooming out doubles the window around its centre, and stays live at the present', () => {
  const now = 1_000_000
  assert.deepEqual(zoomOut(500_000, 600_000, now), { from: 450_000, to: 650_000 })
  // A window whose doubled end would pass now becomes a trailing window.
  assert.deepEqual(zoomOut(now - 3600, now, now), { rangeS: 7200 })
  // One whose doubled end stops short of now stays a fixed window.
  assert.deepEqual(zoomOut(now - 10_000, now - 4_000, now), { from: now - 13_000, to: now - 1_000 })
})

test('a datetime-local value round-trips to the minute, and nonsense is refused', () => {
  const t = 1_793_911_200 // a whole minute
  assert.equal(fromLocalInput(toLocalInput(t)), t)
  assert.equal(fromLocalInput(''), null)
  assert.equal(fromLocalInput('yesterday'), null)
  assert.equal(fromLocalInput('2026-13-45T99:99'), null)
})

test('the time axis says the date once a window is longer than a day', () => {
  const t = new Date(2026, 10, 5, 14, 30).getTime() / 1000
  assert.equal(axisLabel(t, 3600), '14:30')
  assert.equal(axisLabel(t, 86400), '14:30')
  assert.equal(axisLabel(t, 2 * 86400), '5 Nov 14:30')
  assert.equal(axisLabel(t, 30 * 86400), '5 Nov')
})

test('two families of one alert have different keys', () => {
  const a = { rule_id: 1, target_id: 2, agent_id: 3, family: 'v4' }
  assert.notEqual(alertKey(a), alertKey({ ...a, family: 'v6' }))
  assert.equal(alertKey(a), alertKey({ ...a }))
})

test('seriesLabel names agent and family, whichever are known', () => {
  assert.equal(seriesLabel('local', 'v4'), 'local · v4')
  assert.equal(seriesLabel(undefined, 'v6'), 'v6')
  assert.equal(seriesLabel('local', ''), 'local')
})

test('a row never ends in a separator when it has no vantage or family to name', () => {
  assert.equal(afterName(seriesLabel('local', 'v4')), ' · local · v4')
  assert.equal(afterName(seriesLabel(undefined, undefined)), '')
  assert.equal(afterName(undefined), '')
})

test('an alert is named with its series, and without a trailing comma when it has none', () => {
  const a = { rule: 'loss', target: '/Production/web', agent: 'local', family: 'v6' }
  assert.equal(alertName(a), 'loss on /Production/web, local · v6')
  assert.equal(alertName({ rule: 'loss', target: '/Production/web' }), 'loss on /Production/web')
})

// Protects the summary figures from an interval that sent nothing (no data,
// with a reason): it is left out of loss, so one such interval beside a lossy
// one does not dilute the loss and does not count as a clean one, and the flag
// it carries still reaches the series' flags.
test('summarise leaves intervals that sent nothing out of the loss', () => {
  const s = summarise({
    values: new Uint32Array([1000, 2000]),
    sent: new Float64Array([10, 0, 0]),
    received: new Float64Array([5, 0, 0]),
    flags: new Uint8Array([0, 1 << 6, 1 << 6]),
  })
  assert.equal(s.loss, 50)
  assert.equal(s.sent, 10)
  assert.equal(s.flags, 1 << 6)
})

// Protects the window that is only no data: nothing was sent, which callers read
// off `sent` (loss alone would say 0%).
test('summarise of a window with nothing sent says so by sent, with no latency', () => {
  const s = summarise({
    values: new Uint32Array(),
    sent: new Float64Array([0, 0]),
    received: new Float64Array([0, 0]),
    flags: new Uint8Array([1 << 6, 1 << 6]),
  })
  assert.equal(s.sent, 0)
  assert.equal(s.median, null)
})

// A target only as far as the series code reads it.
const target = (address_family: string | null, agents = 'local'): Target =>
  ({ id: 7, address_family, settings: { agents: { effective: agents } } }) as unknown as Target
const ids = new Map([
  ['local', 0],
  ['edge', 3],
])

test('a single-family target is one series per agent with no family', () => {
  for (const af of ['v4', 'v6', null]) {
    const s = resolveSeries(target(af), ids)
    assert.equal(s.length, 1)
    assert.equal(s[0].family, undefined)
  }
})

test('a both target is v4 then v6 for each agent', () => {
  const s = resolveSeries(target('both', 'local edge'), ids)
  assert.deepEqual(
    s.map((x) => [x.agentId, x.family]),
    [
      [0, 'v4'],
      [0, 'v6'],
      [3, 'v4'],
      [3, 'v6'],
    ],
  )
})

test('a both target nobody measures is one unresolved series, not two', () => {
  const s = resolveSeries(target('both', 'ghost'), ids)
  assert.equal(s.length, 1)
  assert.equal(s[0].agentId, null)
  assert.equal(s[0].family, undefined)
})

test('the family filter against v4-only, v6-only and both targets', () => {
  const v4 = target('v4')
  const v6 = target('v6')
  const both = target('both')
  const shows = (c: 'v4' | 'v6' | 'both', t: Target) =>
    filterByFamily(resolveSeries(t, ids), c).map((s) => s.family ?? s.target.address_family)
  assert.deepEqual(shows('both', v4), ['v4'])
  assert.deepEqual(shows('both', v6), ['v6'])
  assert.deepEqual(shows('both', both), ['v4', 'v6'])
  assert.deepEqual(shows('v4', v4), ['v4'])
  assert.deepEqual(shows('v4', v6), [])
  assert.deepEqual(shows('v4', both), ['v4'])
  assert.deepEqual(shows('v6', v4), [])
  assert.deepEqual(shows('v6', v6), ['v6'])
  assert.deepEqual(shows('v6', both), ['v6'])
  // An unresolved both target belongs to both families, so neither choice hides it.
  const ghost = resolveSeries(target('both', 'ghost'), ids)
  assert.equal(filterByFamily(ghost, 'v6').length, 1)
  assert.equal(filterByFamily(ghost, 'v4').length, 1)
  // A group-like target with no family is read as v4, the family the API defaults to.
  assert.equal(showsFamily('v6', target(null)), false)
  assert.equal(showsFamily('v4', target(null)), true)
})

test('shownFamilies is what the detail page draws', () => {
  assert.deepEqual(shownFamilies(target('both'), 'both'), ['v4', 'v6'])
  assert.deepEqual(shownFamilies(target('both'), 'v6'), ['v6'])
  assert.deepEqual(shownFamilies(target('v4'), 'both'), [undefined])
  assert.deepEqual(shownFamilies(target('v4'), 'v6'), [])
})

test('a stored family choice is read back, and anything else is both', () => {
  assert.equal(parseFamilyChoice('v4'), 'v4')
  assert.equal(parseFamilyChoice('v6'), 'v6')
  assert.equal(parseFamilyChoice('both'), 'both')
  assert.equal(parseFamilyChoice(null), 'both')
  assert.equal(parseFamilyChoice('v5'), 'both')
})

test('a series key carries the family, and is unchanged without one', () => {
  assert.equal(seriesKey(7, 3), '7-3')
  assert.equal(seriesKey(7, null), '7-unresolved')
  assert.equal(seriesKey(7, 3, 'v4'), '7-3-v4')
  assert.notEqual(seriesKey(7, 3, 'v4'), seriesKey(7, 3, 'v6'))
})

test('the requests carry family only when it is set', () => {
  assert.equal(seriesURL(7, 3, 10, 20), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20')
  assert.equal(seriesURL(7, 3, 10, 20, 500), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20&columns=500')
  assert.equal(seriesURL(7, 3, 10, 20, 500, 'v6'), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20&columns=500&family=v6')
  assert.equal(pathsURL(7, 3, 10, 20), '/api/v1/paths?target_id=7&agent_id=3&from=10&to=20')
  assert.equal(pathsURL(7, 3, 10, 20, 'v6'), '/api/v1/paths?target_id=7&agent_id=3&from=10&to=20&family=v6')
  assert.equal(availabilityURL(7, 10, 20), '/api/v1/availability?target_id=7&from=10&to=20')
  assert.equal(availabilityURL(7, 10, 20, 100), '/api/v1/availability?target_id=7&from=10&to=20&down_threshold=100')
  assert.equal(availabilityURL(7, 10, 20, 100, 'v6'), '/api/v1/availability?target_id=7&from=10&to=20&down_threshold=100&family=v6')
})

test('the two families of one window are two in-flight requests', async () => {
  const urls: string[] = []
  const real = globalThis.fetch
  globalThis.fetch = (async (u: string) => {
    urls.push(u)
    return new Response('x', { status: 500 })
  }) as typeof fetch
  try {
    await Promise.allSettled([fetchSeries(7, 3, 10, 20, 5, 'v4'), fetchSeries(7, 3, 10, 20, 5, 'v6')])
  } finally {
    globalThis.fetch = real
  }
  assert.equal(urls.length, 2)
})

test('countedFlags leaves out the userspace pair only where the probe type always carries it', () => {
  // irtt, tcp and http(s) are timed in userspace on every interval.
  for (const type of ['irtt', 'tcp', 'http', 'https']) {
    assert.equal(countedFlags(0b11, type), 0, type)
    assert.equal(countedFlags(0b01, type), 0, type)
    // Anything else on the same interval still counts: dropped replies, a clock step.
    assert.equal(countedFlags(0b11 | (1 << 3), type), 1 << 3, type)
    assert.equal(countedFlags(1 << 5, type), 1 << 5, type)
  }
  // icmp and dns are stamped by the kernel, so there the pair means that failed.
  for (const type of ['icmp', 'dns', '', undefined, null]) {
    assert.equal(countedFlags(0b11, type), 0b11, String(type))
    assert.equal(countedFlags(0b10, type), 0b10, String(type))
  }
})

test('firingAnnouncement speaks only when the count rises', () => {
  assert.equal(firingAnnouncement(null, 0), null)
  assert.equal(firingAnnouncement(null, 2), '2 alerts firing')
  assert.equal(firingAnnouncement(0, 1), '1 alert firing')
  assert.equal(firingAnnouncement(1, 3), '3 alerts firing')
  assert.equal(firingAnnouncement(3, 3), null)
  assert.equal(firingAnnouncement(3, 2), null)
})

test('countUnacked skips acknowledged and silenced alerts', () => {
  assert.equal(countUnacked([{}, { acked: true }, { silenced: true }, { acked: false, silenced: false }]), 2)
})

test('measurementLabel puts no data before loss before flags', () => {
  assert.equal(measurementLabel({ sent: 0, loss: 3, flagged: 2 }), 'no data')
  assert.equal(measurementLabel({ sent: 10, loss: 1, flagged: 2 }), 'loss')
  assert.equal(measurementLabel({ sent: 10, loss: 0, flagged: 2 }), 'flagged')
  assert.equal(measurementLabel({ sent: 10, loss: 0, flagged: 0 }), 'clean')
  assert.equal(measurementLabel({ flagged: 1 }), 'flagged')
  assert.equal(measurementLabel({}), 'clean')
})

test('screenTitle names every screen, detail after its target', () => {
  assert.equal(screenTitle('overview'), 'Overview - smokeng')
  assert.equal(screenTitle('access'), 'Access - smokeng')
  assert.equal(screenTitle('signin'), 'Sign in - smokeng')
  assert.equal(screenTitle('detail', 'core/router'), 'core/router - smokeng')
  assert.equal(screenTitle('detail', null), 'Target - smokeng')
})

test('rangeWords speaks a range button', () => {
  assert.deepEqual(RANGES.map((r) => rangeWords(r.seconds)), ['15 minutes', '1 hour', '6 hours', '1 day', '7 days', '30 days'])
  // Twice a custom window of 37 minutes, as zooming out makes it: not rounded.
  assert.equal(rangeWords(4440), '1 hour 14 minutes')
  assert.equal(rangeWords(7200), '2 hours')
  assert.equal(rangeWords(10), '10 seconds')
  assert.equal(rangeWords(90061), '1 day 1 hour 1 minute 1 second')
})

test('mayChange follows the node, and keeps admin-only settings with an admin', () => {
  const editable = { writable: true }
  const readOnly = { writable: false }
  // An editor through a grant: global role viewer, but this node is theirs.
  assert.equal(mayChange(editable, false), true)
  assert.equal(mayChange(editable, false, true), false)
  // A node outside their grant, or a plain viewer.
  assert.equal(mayChange(readOnly, false), false)
  // An admin's listing is writable everywhere; admin-only settings are theirs.
  assert.equal(mayChange(editable, true, true), true)
  // The server's word wins over the role either way.
  assert.equal(mayChange(readOnly, true), false)
})

test('the settings the Targets screen keeps with an admin are the ones the server refuses', () => {
  const go = readFileSync(new URL('../../internal/api/targets.go', import.meta.url), 'utf8')
  const ui = readFileSync(new URL('./Admin.tsx', import.meta.url), 'utf8')
  const server = go.match(/adminOnlySettings = \[\]string\{([^}]*)\}/)?.[1].match(/"[a-z_]+"/g)?.map((s) => s.slice(1, -1))
  const client = [...ui.matchAll(/\{[^{}]*key: '([a-z_]+)'[^{}]*adminOnly: true/g)].map((m) => m[1])
  assert.ok(server && server.length > 0, 'adminOnlySettings not found in targets.go')
  assert.deepEqual(client.sort(), server.sort())
})

test('rows are grouped by parent path, in the order given', () => {
  assert.equal(groupOfPath('/brabonet/sites/web'), 'brabonet/sites')
  assert.equal(groupOfPath('/gateway'), '')
  const rows = ['/a/x', '/gw', '/a/y', '/a/x'].map((path) => ({ target: { path } }))
  assert.deepEqual(
    groupRows(rows).map((g) => [g.path, g.rows.length]),
    [['a', 3], ['', 1]],
  )
})

test('the worst status wins: no data < ok < flagged < loss', () => {
  assert.equal(rowStatus({ sent: 0, loss: 0 }, 0), 'nodata')
  assert.equal(rowStatus({ sent: 5, loss: 0 }, 2), 'flagged')
  assert.equal(rowStatus({ sent: 5, loss: 0.1 }, 2), 'loss')
  assert.equal(worstStatus([]), 'nodata')
  assert.equal(worstStatus(['ok', 'nodata', 'flagged']), 'flagged')
  assert.equal(worstStatus(['flagged', 'loss', 'ok']), 'loss')
})

test('graphGroups groups leaves by parent path in tree order', () => {
  const t = (id: number, path: string, sort_order: number): Target =>
    ({ id, path, sort_order, name: path.split('/').pop(), parent_id: null }) as unknown as Target
  const a = t(1, '/web/b', 2)
  const b = t(2, '/web/a', 1)
  const c = t(3, '/gw', 0)
  assert.equal(graphGroupLabel('/web/a'), 'web')
  assert.equal(graphGroupLabel('/gw'), 'Top level')
  const r = graphGroups([a, b, c], [a, b, c])
  assert.equal(r.grouped, true)
  assert.deepEqual(r.groups.map((g) => [g.label, g.items.map((i) => i.id)]), [['Top level', [3]], ['web', [2, 1]]])
  assert.equal(graphGroups([c], [c]).grouped, false)
})

test('flagsIn names the flags that count, and leaves out the userspace pair where it is expected', () => {
  const cut = 1 << 7
  const userspace = 0b11
  assert.deepEqual(flagsIn(cut | userspace, 'icmp').map((f) => f.label), ['userspace TX', 'userspace RX', 'cut short'])
  assert.deepEqual(flagsIn(cut | userspace, 'irtt').map((f) => f.label), ['cut short'])
  assert.deepEqual(flagsIn(userspace, 'https'), [])
  assert.deepEqual(flagsIn(0, 'icmp'), [])
})

test('flagCounts counts intervals per flag and keeps the start of the last one', () => {
  const cut = 1 << 7
  const notSent = 1 << 6
  const s = { ts: [100, 400, 700, 1000], flags: [0, cut | 0b11, notSent, cut] }
  assert.deepEqual(flagCounts(s, 'irtt'), [
    { label: 'cut short', title: flagsIn(cut, 'irtt')[0].title, count: 2, last: 1000 },
    { label: 'not sent', title: flagsIn(notSent, 'irtt')[0].title, count: 1, last: 700 },
  ])
  // On icmp the userspace pair counts, once.
  assert.deepEqual(flagCounts(s, 'icmp').map((f) => [f.label, f.count]), [
    ['userspace TX', 1],
    ['userspace RX', 1],
    ['cut short', 2],
    ['not sent', 1],
  ])
  assert.deepEqual(flagCounts({ ts: [1, 2], flags: [0, 0] }, 'icmp'), [])
})

test('flagNote says how many intervals of how many, and when', () => {
  assert.equal(flagNote({ label: 'cut short', count: 1 }, 12, '22:25'), 'cut short in 1 of 12 intervals, last at 22:25')
  assert.equal(flagNote({ label: 'not sent', count: 12 }, 12, '22:25'), 'not sent in every interval')
})

test('the shared flag list names every bit of the flags byte', () => {
  // store.Flag* fills all eight bits. A bit missing here is a flag the overview
  // counts against a series without ever saying what it was.
  assert.equal(QUALITY_FLAGS.reduce((a, f) => a | f.bit, 0), 0xff)
  assert.equal(new Set(QUALITY_FLAGS.map((f) => f.bit)).size, QUALITY_FLAGS.length)
})

test('flagCounts says nothing for a pooled read, whose rows are columns', () => {
  assert.deepEqual(flagCounts({ ts: [1], flags: [1 << 7], pooled: {} }, 'icmp'), [])
})

test('flagsIn names ICMP errors, which count against a series', () => {
  assert.deepEqual(flagsIn(1 << 4, 'icmp').map((f) => f.label), ['ICMP errors'])
})
