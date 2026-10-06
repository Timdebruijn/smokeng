import assert from 'node:assert/strict'
import { test } from 'node:test'
import { columnFromArray, dictionary, int32, int64, list, tableFromColumns, tableToIPC, timestamp, uint16, uint32, uint8, utf8 } from '@uwdata/flechette'
import { decodeSeries, plausibleRows } from './api.ts'
import { MAX_SCHEMA_NODES, screenIPC } from './ipc.ts'

// What the server sends for a range of measurements, in the column types it
// writes: a timestamp, small unsigned integers, and a list of uint32.
type Columns = Record<string, ReturnType<typeof columnFromArray>>
function table(columns: Columns): Uint8Array {
  return tableToIPC(tableFromColumns(columns), { format: 'stream' })!
}
const base: Columns = {
  ts: columnFromArray([1_756_400_000_000, 1_756_400_060_000, 1_756_400_120_000], timestamp(1)),
  sent: columnFromArray([20, 20, 20], uint16()),
  received: columnFromArray([3, 2, 0], uint16()),
  flags: columnFromArray([0, 0, 1], uint8()),
  samples: columnFromArray(
    [new Uint32Array([1, 2, 3]), new Uint32Array([4, 5]), new Uint32Array([])],
    list(uint32()),
  ),
}

test('a valid table decodes to the series it describes', () => {
  const s = decodeSeries(table(base))
  assert.deepEqual([...s.ts], [1_756_400_000, 1_756_400_060, 1_756_400_120])
  assert.deepEqual([...s.sent], [20, 20, 20])
  assert.deepEqual([...s.received], [3, 2, 0])
  assert.deepEqual([...s.flags], [0, 0, 1])
  assert.deepEqual([...s.offsets], [0, 3, 5, 5])
  assert.deepEqual([...s.values], [1, 2, 3, 4, 5])
})

// Everything the page does with a measurement response happens inside a promise
// whose rejection becomes a message under the plot. A TypeError from reading a
// column that is not there, or a RangeError from flechette on bad bytes, is not
// worded for anyone and says nothing about what to do.
test('what is not a measurement table is an error that says so', () => {
  const stringy = table({ ...base, extra: columnFromArray(['a', 'b', 'c'], utf8()) })
  const { samples: _, ...noSamples } = base
  for (const [name, bytes] of [
    ['empty', new Uint8Array(0)],
    ['not arrow', new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])],
    ['text', new TextEncoder().encode('<html>502 Bad Gateway</html>')],
    ['a column missing', table(noSamples)],
    ['a column type the server never writes', stringy],
  ] as const) {
    assert.throws(() => decodeSeries(bytes), (e: unknown) => e instanceof Error && e.message.startsWith('measurements: '), name)
  }
})

// What the full schema looks like: error columns with nulls, and the extra
// per-packet series, which are lists of signed integers and null where a probe
// measures nothing of the sort.
const rich: Columns = {
  ...base,
  target_id: columnFromArray([7, 7, 7], int64()),
  icmp_error: columnFromArray([null, 3, null], uint16()),
  send_error: columnFromArray([null, null, 1], uint8()),
  ipdv_send: columnFromArray([new Int32Array([-4, 4]), null, new Int32Array([])], list(int32())),
  server_processing: columnFromArray([null, null, null], list(int32())),
}

// One bad byte on the wire (a proxy, a truncated download, a bug) must not be
// able to do anything but produce a series or a worded error: not a hang, not a
// huge allocation, not an exception of some other kind. Before the stream was
// screened, setting one byte to 5 in a 700-byte response filled a 4 GB heap and
// aborted the process, which in a browser is a dead tab.
function assertSeriesOrWordedError(bad: Uint8Array, what: string): 'decoded' | 'refused' {
  try {
    const s = decodeSeries(bad)
    const n = s.ts.length
    assert.equal(s.sent.length, n)
    assert.equal(s.received.length, n)
    assert.equal(s.flags.length, n)
    assert.equal(s.offsets.length, n + 1)
    assert.equal(s.values.length, s.offsets[n])
    return 'decoded'
  } catch (e) {
    assert.ok(e instanceof Error && e.message.startsWith('measurements: '),
      `${what}: ${e instanceof Error ? `${e.name}: ${e.message}` : String(e)}`)
    return 'refused'
  }
}

for (const [name, columns] of [['the required columns', base], ['the full schema', rich]] as const) {
  test(`corrupting any single byte of ${name} yields a series or a worded error, quickly`, () => {
    const good = table(columns)
    assert.equal(assertSeriesOrWordedError(good, 'the uncorrupted table'), 'decoded')
    const started = Date.now()
    const seen = { decoded: 0, refused: 0 }
    for (let i = 0; i < good.length; i++) {
      for (const v of [0x00, 0xff, 0x7f, good[i] ^ 0x01, good[i] ^ 0x80]) {
        const bad = good.slice()
        bad[i] = v
        seen[assertSeriesOrWordedError(bad, `byte ${i} set to ${v}`)]++
      }
    }
    assert.ok(seen.refused > 0 && seen.decoded > 0, `expected both outcomes, got ${JSON.stringify(seen)}`)
    assert.ok(Date.now() - started < 20_000, 'decoding corrupted input took too long')
  })

  test(`corrupting several bytes of ${name} at once yields a series or a worded error`, () => {
    const good = table(columns)
    let state = 12345 // a fixed sequence, so a failure can be reproduced
    const next = (n: number) => {
      state = (Math.imul(state, 1103515245) + 12345) >>> 0
      return state % n
    }
    const cases = Number(process.env.FUZZ_CASES ?? 3000) // raise it to hunt
    for (let k = 0; k < cases; k++) {
      const bad = good.slice()
      for (let j = 1 + next(4); j > 0; j--) bad[next(bad.length)] = next(256)
      assertSeriesOrWordedError(bad, `case ${k}`)
    }
  })
}

test('truncating at any length yields a series or a worded error', () => {
  const good = table(base)
  for (let len = 0; len < good.length; len++) {
    try {
      decodeSeries(good.slice(0, len))
    } catch (e) {
      assert.ok(e instanceof Error && e.message.startsWith('measurements: '), `length ${len}: ${String(e)}`)
    }
  }
})

// A row costs the sender bytes, so a table that claims more rows than the
// response has bytes is lying, and the arrays sized from the claim would be
// allocated for nothing.
test('a row count the body cannot hold is refused', () => {
  assert.equal(plausibleRows(0, 0), true)
  assert.equal(plausibleRows(3, 800), true)
  assert.equal(plausibleRows(200_000, 7_000_000), true)
  assert.equal(plausibleRows(801, 800), false)
  assert.equal(plausibleRows(2 ** 31, 800), false)
  assert.equal(plausibleRows(-1, 800), false)
  assert.equal(plausibleRows(NaN, 800), false)
})

function indexOfInt32s(bytes: Uint8Array, values: number[]): number {
  const want = new Uint8Array(new Int32Array(values).buffer)
  outer: for (let i = 0; i + want.length <= bytes.length; i++) {
    for (let j = 0; j < want.length; j++) if (bytes[i + j] !== want[j]) continue outer
    return i
  }
  return -1
}

// A list column is a buffer of offsets into its values. The offsets sit in the
// body, where no header check can see them, and flechette sizes the array it
// builds for a row from the difference of two of them: an offset of two billion
// is a request for two billion elements, from one corrupt byte.
test('list offsets that do not fit their values are refused, not expanded', () => {
  const good = table(base) // the samples offsets are 0, 3, 5, 5
  const at = indexOfInt32s(good, [0, 3, 5, 5])
  assert.ok(at >= 0, 'the offsets of the fixture were not found')
  const patch = (nth: number, value: number) => {
    const bad = good.slice()
    new DataView(bad.buffer).setInt32(at + 4 * nth, value, true)
    return bad
  }
  for (const [what, bad] of [
    ['an offset past the values', patch(2, 0x7fffffff)],
    ['an offset that is huge but not the last', patch(1, 0x40000000)],
    ['the last offset past the values', patch(3, 0x7fffffff)],
    ['a negative offset', patch(1, -1)],
    ['a first offset that is not zero', patch(0, 2)],
  ] as const) {
    const started = Date.now()
    try {
      decodeSeries(bad)
      assert.fail(`${what} was accepted`)
    } catch (e) {
      assert.ok(e instanceof Error && e.message.startsWith('measurements: '), `${what}: ${String(e)}`)
    }
    assert.ok(Date.now() - started < 2000, `${what} took ${Date.now() - started} ms`)
  }
  // Decreasing: 0, 3, 2, 5 (the third is lower than the second).
  const dec = good.slice()
  new DataView(dec.buffer).setInt32(at + 8, 2, true)
  assert.throws(() => decodeSeries(dec), /^Error: measurements: /)
})

// The extra series columns are lists too, and are read after the same check.
test('the offsets of an extra series column are checked as well', () => {
  const good = table(rich) // ipdv_send: [-4, 4], null, []  ->  offsets 0, 2, 2, 2
  const at = indexOfInt32s(good, [0, 2, 2, 2])
  assert.ok(at >= 0, 'the offsets of the ipdv_send fixture were not found')
  assert.doesNotThrow(() => decodeSeries(good))
  const bad = good.slice()
  new DataView(bad.buffer).setInt32(at + 12, 0x7fffffff, true)
  assert.throws(() => decodeSeries(bad), /^Error: measurements: .*ipdv_send/)
})

// The schema is counted by what it expands to; this many columns is more than
// the server has ever written, and the budget is the whole of it.
test('a schema is refused past its budget of fields', () => {
  const wide = (n: number): Columns => {
    const cols: Columns = {}
    for (let i = 0; i < n; i++) cols[`c${i}`] = columnFromArray([1], uint8())
    return cols
  }
  assert.doesNotThrow(() => screenIPC(table(wide(MAX_SCHEMA_NODES))))
  assert.throws(() => screenIPC(table(wide(MAX_SCHEMA_NODES + 1))), /more than \d+ fields/)
  assert.throws(() => decodeSeries(table(wide(MAX_SCHEMA_NODES + 1))), /^Error: measurements: /)
})

// Nested columns are the other way to spend the budget: a list of lists of lists.
test('nesting is refused past its depth', () => {
  const deep = columnFromArray([[[[new Uint8Array([1])]]]] as never, list(list(list(list(uint8())))))
  assert.throws(() => screenIPC(table({ deep })), /nested too deep/)
  const shallow = columnFromArray([[new Uint8Array([1])]] as never, list(list(uint8())))
  assert.doesNotThrow(() => screenIPC(table({ shallow })))
})

// Dictionaries are never written, and a stream with one sends a message of a
// type the server does not use.
test('a dictionary-encoded column is refused', () => {
  const bytes = table({ d: columnFromArray([1, 2, 1], dictionary(int32())) })
  assert.throws(() => screenIPC(bytes), /^Error: measurements: /)
})

// The messages of a stream, found by their framing, so a test can repeat or
// reorder them.
function messages(bytes: Uint8Array): Uint8Array[] {
  const out: Uint8Array[] = []
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  let pos = 0
  while (pos < bytes.length) {
    const metaLen = dv.getUint32(pos + 4, true)
    if (metaLen === 0) break // end of stream
    // The body length is the int64 in the message; every message here is
    // followed by its body, so take what lies before the next marker.
    let end = pos + 8 + metaLen
    while (end < bytes.length && !(dv.getUint32(end, true) === 0xffffffff && end % 8 === 0)) end += 8
    out.push(bytes.slice(pos, end))
    pos = end
  }
  return out
}
function join(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0) + 8)
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  out.set([0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0], at) // end of stream
  return out
}

test('the framing helper reproduces the stream it takes apart', () => {
  const good = table(base)
  const [schema, batch] = messages(good)
  assert.deepEqual(join(schema, batch), good)
  assert.doesNotThrow(() => decodeSeries(join(schema, batch)))
})

// An empty batch costs the sender a few dozen bytes and the reader a decode.
test('a stream of very many messages is refused', () => {
  const [schema, batch] = messages(table(base))
  assert.doesNotThrow(() => screenIPC(join(schema, batch, batch, batch)))
  assert.throws(() => screenIPC(join(schema, ...Array(1100).fill(batch))), /more than \d+ messages/)
})

test('a second schema, or a batch with none before it, is refused', () => {
  const [schema, batch] = messages(table(base))
  assert.throws(() => screenIPC(join(schema, schema, batch)), /schema that is not first/)
  assert.throws(() => screenIPC(join(batch, schema)), /^Error: measurements: /)
})
