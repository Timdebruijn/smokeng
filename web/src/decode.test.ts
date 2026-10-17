import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { decodeSeries, SERIES_NAMES, type Series } from './api.ts'
import { MAX_FIELDS, MAX_MESSAGES, MAX_ROWS } from './arrow.ts'

// The fixtures in testdata/ are written by the Go server's own writer
// (internal/api/web_fixtures_test.go regenerates them and fails if they drift),
// each with the values it holds in a .json beside it. So these tests check the
// reader against what the real writer produces and against values that came from
// the other side, not against a description of the format written here.

const dir = new URL('../testdata/', import.meta.url)
const load = (name: string) => new Uint8Array(readFileSync(new URL(name, dir)))
interface Expected {
  ts: number[]
  sent: number[]
  received: number[]
  flags: number[]
  samples: number[][]
  icmp_error: (number | null)[]
  send_error: (number | null)[]
  series: Record<string, (number[] | null)[]>
  batches: number
}
const expected = (name: string): Expected => JSON.parse(readFileSync(new URL(`${name}.json`, dir), 'utf8'))
const NAMES = ['empty', 'one', 'small', 'multi', 'wide'] as const

function rows(s: Series): number[][] {
  return Array.from({ length: s.ts.length }, (_, i) => Array.from(s.values.subarray(s.offsets[i], s.offsets[i + 1])))
}

// ---- what the reader returns for what the writer wrote ----------------------

for (const name of NAMES) {
  test(`${name}: decodes to exactly the values the writer was given`, () => {
    const want = expected(name)
    const s = decodeSeries(load(`${name}.arrow`))
    assert.deepEqual(Array.from(s.ts), want.ts)
    assert.deepEqual(Array.from(s.sent), want.sent)
    assert.deepEqual(Array.from(s.received), want.received)
    assert.deepEqual(Array.from(s.flags), want.flags)
    assert.deepEqual(rows(s), want.samples)
    assert.deepEqual(s.icmpErrors, want.icmp_error)
    assert.deepEqual(s.sendErrors, want.send_error)
    for (const series of SERIES_NAMES) {
      const perRow = want.series[series]
      const measured = perRow.some((r) => r !== null)
      const got = s.extra[series]
      // A series no interval carried is left out: an empty graph would invite
      // the reader to conclude the jitter was zero.
      assert.equal(got !== undefined, measured, `${series} present`)
      if (!got) continue
      assert.deepEqual(Array.from(got.measured), perRow.map((r) => (r === null ? 0 : 1)))
      const flat = Array.from({ length: perRow.length }, (_, i) => Array.from(got.values.subarray(got.offsets[i], got.offsets[i + 1])))
      assert.deepEqual(flat, perRow.map((r) => r ?? []))
    }
  })
}

test('the fixtures cover what the reader has to handle', () => {
  assert.equal(decodeSeries(load('empty.arrow')).ts.length, 0)
  assert.equal(expected('multi').batches, 3) // more than one record batch
  assert.ok(Object.keys(decodeSeries(load('small.arrow')).extra).length > 0, 'extra series with nulls')
  assert.equal(Object.keys(decodeSeries(load('one.arrow')).extra).length, 0, 'no extra series at all')
  const small = decodeSeries(load('small.arrow'))
  assert.ok(small.icmpErrors.includes(null) && small.icmpErrors.some((v) => v !== null), 'nullable columns, both ways')
  assert.equal(decodeSeries(load('wide.arrow')).ts.length, 20) // validity bitmaps of more than one byte
})

test('a column the reader has no use for is skipped; a time unit other than seconds is converted', () => {
  const extra = decodeSeries(load('extra-int-column.arrow'))
  assert.equal(extra.ts.length, 1)
  assert.equal(extra.ts[0], 1_756_400_000)
  // The same number written as milliseconds is a thousand times earlier.
  assert.equal(decodeSeries(load('ts-in-millis.arrow')).ts[0], 1_756_400)
})

// ---- where things are, found by an independent walk ---------------------------

interface Msg {
  pos: number
  metaPos: number
  metaLen: number
  type: number
  typePos: number
  headerPos: number
  vtablePos: number
  bodyPos: number
  bodyLen: number
  bodyLenPos: number
  rowsPos?: number
  nodesCountPos?: number
  bufsCountPos?: number
  fieldsCountPos?: number // of the schema
  nodes: number[] // positions of each node: length at +0, null count at +8
  bufs: number[] // positions of each buffer: offset at +0, length at +8
}

function layout(b: Uint8Array): Msg[] {
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const out: Msg[] = []
  const slotsOf = (table: number) => {
    const vtable = table - dv.getInt32(table, true)
    const size = dv.getUint16(vtable, true)
    return { vtable, at: (v: number) => (v < size && dv.getUint16(vtable + v, true) ? table + dv.getUint16(vtable + v, true) : -1) }
  }
  const follow = (at: number) => at + dv.getUint32(at, true)
  let pos = 0
  while (pos < b.length) {
    const metaLen = dv.getUint32(pos + 4, true)
    if (metaLen === 0) break
    const metaPos = pos + 8
    const root = follow(metaPos)
    const r = slotsOf(root)
    const typePos = r.at(6)
    const type = b[typePos]
    const bodyLenPos = r.at(10)
    const bodyLen = bodyLenPos >= 0 ? Number(dv.getBigUint64(bodyLenPos, true)) : 0
    const headerPos = r.at(8)
    const m: Msg = { pos, metaPos, metaLen, type, typePos, headerPos, vtablePos: r.vtable, bodyPos: metaPos + metaLen, bodyLen, bodyLenPos, nodes: [], bufs: [] }
    const table = follow(headerPos)
    const t = slotsOf(table)
    if (type === 3) {
      m.rowsPos = t.at(4)
      const nodes = follow(t.at(6))
      const bufs = follow(t.at(8))
      m.nodesCountPos = nodes
      m.bufsCountPos = bufs
      for (let i = 0; i < dv.getUint32(nodes, true); i++) m.nodes.push(nodes + 4 + 16 * i)
      for (let i = 0; i < dv.getUint32(bufs, true); i++) m.bufs.push(bufs + 4 + 16 * i)
    } else if (type === 1) {
      m.fieldsCountPos = follow(t.at(6))
    }
    out.push(m)
    pos = m.bodyPos + bodyLen
  }
  return out
}

/** Where the i'th column's Field table is, found the same way. */
function fieldTable(b: Uint8Array, i: number) {
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const m = layout(b)[0]
  const follow = (at: number) => at + dv.getUint32(at, true)
  const slot = (table: number, v: number) => {
    const vt = table - dv.getInt32(table, true)
    return v < dv.getUint16(vt, true) && dv.getUint16(vt + v, true) ? table + dv.getUint16(vt + v, true) : -1
  }
  const schema = follow(m.headerPos)
  const vec = follow(slot(schema, 6))
  const field = follow(vec + 4 + 4 * i)
  return { dv, follow, slot, field }
}

const patched = (b: Uint8Array, edits: [number, number][]) => {
  const out = b.slice()
  for (const [at, v] of edits) out[at] = v
  return out
}
const setU16 = (b: Uint8Array, at: number, v: number) => patched(b, [[at, v & 255], [at + 1, v >> 8]])
const setU32 = (b: Uint8Array, at: number, v: number) => patched(b, [[at, v & 255], [at + 1, (v >>> 8) & 255], [at + 2, (v >>> 16) & 255], [at + 3, (v >>> 24) & 255]])
const setU64 = (b: Uint8Array, at: number, v: bigint) => {
  const out = b.slice()
  new DataView(out.buffer).setBigUint64(at, v, true)
  return out
}
const setI32 = (b: Uint8Array, at: number, v: number) => {
  const out = b.slice()
  new DataView(out.buffer).setInt32(at, v, true)
  return out
}
/** Overwrite a column name with another of the same length. */
function rename(b: Uint8Array, from: string, to: string) {
  assert.equal(from.length, to.length)
  const enc = new TextEncoder()
  const f = enc.encode(from)
  const hits: number[] = []
  const end = layout(b)[1].pos // the schema is the first message
  for (let i = 0; i + f.length <= end; i++) if (f.every((x, k) => b[i + k] === x)) hits.push(i)
  assert.equal(hits.length, 1, `"${from}" occurs ${hits.length} times in the schema`)
  const out = b.slice()
  out.set(enc.encode(to), hits[0])
  return out
}

test('the layout walk finds what the writer wrote', () => {
  const small = layout(load('small.arrow'))
  assert.deepEqual(small.map((m) => m.type), [1, 3])
  assert.equal(small[1].nodes.length, 14)
  assert.equal(small[1].bufs.length, 28)
  assert.equal(new DataView(load('small.arrow').buffer).getUint32(small[0].fieldsCountPos!, true), 10)
  assert.deepEqual(layout(load('multi.arrow')).map((m) => m.type), [1, 3, 3, 3])
})

// ---- the properties every result must have, whatever the bytes ----------------

/** Whatever decodes has to be a coherent series, with no number made of nothing. */
function assertCoherent(s: Series, what: string) {
  const n = s.ts.length
  assert.equal(s.sent.length, n, what)
  assert.equal(s.received.length, n, what)
  assert.equal(s.flags.length, n, what)
  assert.equal(s.icmpErrors.length, n, what)
  assert.equal(s.sendErrors.length, n, what)
  assert.equal(s.offsets.length, n + 1, what)
  assert.equal(s.offsets[0], 0, what)
  assert.equal(s.values.length, s.offsets[n], what)
  for (let i = 0; i < n; i++) {
    assert.ok(Number.isFinite(s.ts[i]) && Number.isFinite(s.sent[i]) && Number.isFinite(s.received[i]), `${what}: row ${i} has a number that is not a number`)
    assert.ok(s.offsets[i] <= s.offsets[i + 1], what)
  }
  for (const e of Object.values(s.extra)) {
    assert.equal(e.offsets.length, n + 1, what)
    assert.equal(e.measured.length, n, what)
    assert.equal(e.values.length, e.offsets[n], what)
  }
}

/**
 * What decoding the bytes gives: a coherent series, or an error the reader
 * raised on purpose. Not the catch-all in decodeSeries that wraps whatever else
 * was thrown: a RangeError out of a DataView means the reader read something it
 * had not checked, which is a bug even though the page would show a message.
 */
function outcome(bytes: Uint8Array, what: string): 'decoded' | 'refused' {
  try {
    assertCoherent(decodeSeries(bytes), what)
    return 'decoded'
  } catch (e) {
    if (e instanceof assert.AssertionError) throw e
    assert.ok(e instanceof Error && e.message.startsWith('measurements: malformed response (') || e instanceof Error && /^measurements: the response has no /.test(e.message),
      `${what}: ${e instanceof Error ? `${e.name}: ${e.message}` : String(e)}`)
    return 'refused'
  }
}

function refused(bytes: Uint8Array, pattern: RegExp, what: string) {
  assert.throws(() => decodeSeries(bytes), (e: unknown) => e instanceof Error && e.message.startsWith('measurements: ') && pattern.test(e.message), what)
}

// ---- every rule the reader has, shown to fire on its own ----------------------

test('what is not a measurement stream is an error that says so', () => {
  const small = load('small.arrow')
  const enc = new TextEncoder()
  for (const [name, bytes] of [
    ['empty', new Uint8Array(0)],
    ['not arrow', new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])],
    ['an error page', enc.encode('<html>502 Bad Gateway</html>')],
    ['the file format (ARROW1 magic)', new Uint8Array([...enc.encode('ARROW1\0\0'), ...small])],
    ['json', enc.encode('{"error":"nope"}')],
  ] as const) {
    assert.equal(outcome(bytes, name), 'refused', name)
  }
  refused(new Uint8Array([0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0]), /no schema/, 'an end marker and nothing else')
})

test('the framing rules', () => {
  const small = load('small.arrow')
  const [schema, batch] = layout(small)
  refused(patched(small, [[3, 0x7f]]), /continuation marker/, 'a marker that is not one')
  refused(setU32(small, 4, 0x7fffffff), /header longer than the response/, 'a header that runs past the end')
  refused(setU32(small, batch.pos + 4, 0xffffffff), /header longer than the response/, 'a header length of -1')
  refused(setU64(small, batch.bodyLenPos, 1n << 40n), /body longer than the response/, 'a body that runs past the end')
  refused(setU64(small, batch.bodyLenPos, 0xffffffffffffffffn), /cannot be real/, 'a body length of -1')
  refused(setU64(small, batch.bodyLenPos, 1n << 62n), /cannot be real/, 'a body length no double holds exactly')
  refused(setU64(small, batch.bodyLenPos, 1n << 54n), /cannot be real/, 'a body length just past what a double holds')
  refused(setU64(small, batch.bodyLenPos, 1n << 59n), /cannot be real/, 'a body length of 2^59')
  refused(patched(small, [[batch.typePos, 2]]), /message of type 2/, 'a message that is neither schema nor batch')
  refused(setU16(small, schema.vtablePos, 2), /vtable that is not one/, 'a vtable too small to be one')
  refused(setU16(small, schema.vtablePos + 6, 0xffff), /field outside the header/, 'a field pointing out of the response')
  refused(setU32(small, schema.headerPos, 0x7fffffff), /offset outside the header/, 'an offset pointing out of the response')
  refused(setU16(small, schema.vtablePos + 8, 0), /schema message without a schema/, 'a schema message that holds none')
  refused(setU16(small, batch.vtablePos + 8, 0), /batch message without a batch/, 'a batch message that holds none')
  refused(setU16(small, schema.vtablePos + 6, 0), /message of type 0/, 'a message with no type')
})

test('a response cut short is refused at every length, not shown as a shorter series', () => {
  for (const name of NAMES) {
    const good = load(`${name}.arrow`)
    for (let len = 0; len < good.length; len++) {
      assert.equal(outcome(good.slice(0, len), `${name} cut at ${len}`), 'refused', `${name} cut at ${len} of ${good.length}`)
    }
    assert.equal(outcome(good, name), 'decoded')
  }
  const multi = load('multi.arrow')
  refused(multi.slice(0, multi.length - 8), /cut short/, 'no end marker')
})

test('the end-of-stream marker has to be last', () => {
  const multi = load('multi.arrow')
  refused(new Uint8Array([...multi, 0]), /after the end/, 'a byte after the end')
  refused(new Uint8Array([...multi, ...multi]), /after the end/, 'a second stream after the first')
})

test('messages out of order or in excess are refused', () => {
  const m = load('multi.arrow')
  const l = layout(m)
  const end = m.subarray(m.length - 8)
  const schema = m.subarray(l[0].pos, l[1].pos)
  const batch = m.subarray(l[1].pos, l[2].pos)
  const cat = (...parts: Uint8Array[]) => Uint8Array.from(parts.flatMap((p) => Array.from(p)))
  refused(cat(batch, schema, end), /before any schema/, 'a batch before the schema')
  refused(cat(schema, schema, batch, end), /schema that is not first/, 'a second schema')
  // A schema with no batches is an empty series, not an error.
  assert.equal(decodeSeries(cat(schema, end)).ts.length, 0)
  assert.ok(MAX_MESSAGES < 100_000)
  refused(cat(schema, ...Array(MAX_MESSAGES + 1).fill(batch), end), /too many messages/, 'more messages than a response has')
  assert.equal(decodeSeries(cat(schema, ...Array(MAX_MESSAGES - 1).fill(batch), end)).ts.length, 3 * (MAX_MESSAGES - 1))
})

test('the schema rules', () => {
  const small = load('small.arrow')
  const [schema] = layout(small)
  refused(setU32(small, schema.fieldsCountPos!, 0), /schema of 0 fields/, 'no fields')
  refused(setU32(small, schema.fieldsCountPos!, MAX_FIELDS + 1), /schema of 65 fields/, 'too many fields')
  refused(rename(small, 'send_error', 'icmp_error'), /two columns named icmp_error/, 'a duplicated name')
  refused(rename(small, 'received', 'receivex'), /no received column of the right type/, 'a required column that is not there')
  refused(rename(small, 'samples', 'sampleX'), /no samples column/, 'the samples column renamed')
  // A list column's child count is exactly one.
  const f = fieldTable(small, 4) // samples
  const children = f.follow(f.slot(f.field, 14))
  refused(setU32(small, children, 2), /list without exactly one child/, 'a list with two children')
  refused(setU32(small, children, 0), /list without exactly one child/, 'a list with none')
  // The time unit is one of four.
  const millis = load('ts-in-millis.arrow')
  const ts = fieldTable(millis, 0)
  const tsType = ts.follow(ts.slot(ts.field, 10))
  const unitPos = ts.slot(tsType, 4)
  assert.ok(unitPos >= 0, 'the fixture writes its unit')
  for (const unit of [4, 7, 0x7fff, 0xffff]) refused(setU16(millis, unitPos, unit), /time unit/, `a unit of ${unit}`)
})

test('the types the server never writes are refused, where they would change a value', () => {
  const cases: [string, RegExp][] = [
    ['samples-u64', /integer of 64 bits/],
    ['samples-signed', /samples that are not unsigned 32-bit/],
    ['samples-u16', /samples that are not unsigned 32-bit/],
    ['samples-flat', /no samples column of the right type/],
    ['series-unsigned', /ipdv_send column is not signed 32-bit/],
    ['sent-u64', /integer of 64 bits/],
    ['sent-float', /column of type 3/],
    ['flags-u16', /flags that are not an unsigned byte/],
    ['flags-i8', /flags that are not an unsigned byte/],
    ['series-i16', /ipdv_send column is not signed 32-bit/],
    ['extra-utf8', /column of type 5/],
    ['extra-dictionary', /dictionary-encoded/],
    ['samples-of-structs', /list of something other than integers/],
  ]
  for (const [name, pattern] of cases) refused(load(`${name}.arrow`), pattern, name)
})

test('an optional column of the wrong kind is left out, not read as something it is not', () => {
  const s = decodeSeries(load('series-flat.arrow')) // ipdv_send written as a plain integer
  assert.equal(s.ts.length, 1)
  assert.equal(s.extra.ipdv_send, undefined)
})

test('a compressed batch is refused', () => {
  const small = load('small.arrow')
  const batch = layout(small)[1]
  // The batch table's vtable has no slot for compression. Growing it by a slot
  // makes the next two bytes (the table's own offset to its vtable, which sits
  // right after it) read as one.
  const size = new DataView(small.buffer).getUint16(batch.vtablePos, true)
  void size
  const dv = new DataView(small.buffer, small.byteOffset, small.byteLength)
  const root = batch.metaPos + dv.getUint32(batch.metaPos, true)
  const hdr = root + dv.getUint16(root - dv.getInt32(root, true) + 8, true)
  const table = hdr + dv.getUint32(hdr, true)
  const vtable = table - dv.getInt32(table, true)
  assert.equal(dv.getUint16(vtable, true), 10, 'the batch vtable has the three slots the writer fills')
  assert.equal(vtable + 10, table, 'and sits right before its table')
  refused(setU16(small, vtable, 12), /compressed/, 'a compression slot')
})

test('the batch must match the schema, in counts and in bytes', () => {
  const small = load('small.arrow')
  const b = layout(small)[1]
  refused(setU32(small, b.nodesCountPos!, 13), /does not match the schema/, 'a node missing')
  refused(setU32(small, b.bufsCountPos!, 27), /does not match the schema/, 'a buffer missing')
  refused(setU64(small, b.rowsPos!, BigInt(MAX_ROWS + 1)), /more rows than one response/, 'more rows than may be carried')
  refused(setU64(small, b.rowsPos!, 0xffffffffffffffffn), /cannot be real/, 'a row count of -1')
  for (const claim of [0, 6, 8]) {
    refused(setU64(small, b.rowsPos!, BigInt(claim)), /not the batch length/, `the batch says ${claim} rows, the columns say 7`)
    refused(setU64(small, b.nodes[0], BigInt(claim)), /not the batch length/, `ts says ${claim} rows, the batch says 7`)
  }
  refused(setU64(small, b.bufs[1], 1n << 40n), /outside the body/, 'a buffer that starts past the body')
  refused(setU64(small, b.bufs[1] + 8, 1n << 40n), /outside the body/, 'a buffer that runs past the body')
  refused(setU64(small, b.bufs[1] + 8, 0xffffffffffffffffn), /cannot be real/, 'a buffer of length -1')
  // A column shorter than its rows, however the length is written.
  refused(setU64(small, b.bufs[1] + 8, 0n), /shorter than its rows/, 'ts with no data')
  refused(setU64(small, b.bufs[3] + 8, 0n), /shorter than its rows/, 'sent with no data')
  refused(setU64(small, b.bufs[3] + 8, 13n), /shorter than its rows/, 'sent one value short')
  refused(setU64(small, b.bufs[11] + 8, 0n), /shorter than its rows/, 'the samples values with no data')
})

test('validity bitmaps and null counts are believable', () => {
  const small = load('small.arrow')
  const b = layout(small)[1]
  refused(setU64(small, b.nodes[0] + 8, 1n), /no validity bitmap/, 'ts claims a null and has no bitmap')
  refused(setU64(small, b.nodes[6] + 8, 99n), /more nulls than values/, 'icmp_error claims 99 nulls in 7 rows')
  refused(setU64(small, b.nodes[5] + 8, 1n), /list of nullable values/, 'the samples values claim a null')
  const wide = load('wide.arrow')
  const w = layout(wide)[1]
  refused(setU64(wide, w.bufs[12] + 8, 2n), /validity bitmap shorter than its rows/, 'a bitmap of 2 bytes for 20 rows')
  assert.equal(outcome(setU64(wide, w.bufs[12] + 8, 3n), 'a bitmap of exactly 3 bytes'), 'decoded')
})

test('a null in a column every interval has is refused, not read as the zero in its slot', () => {
  const bytes = load('null-received.arrow')
  refused(bytes, /null in the received column/, 'null-received')
  // Control: the same stream with the null made valid decodes, so the refusal is
  // about the null and not about the stream.
  const b = layout(bytes)[1]
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  const at = b.bodyPos + Number(dv.getBigInt64(b.bufs[4], true)) // received's validity bitmap
  const s = decodeSeries(patched(bytes, [[at, 0xff]]))
  assert.equal(s.ts.length, 2)
})

test('a timestamp no double can hold exactly is refused, not rounded', () => {
  const small = load('small.arrow')
  const b = layout(small)[1]
  const dv = new DataView(small.buffer, small.byteOffset, small.byteLength)
  const at = b.bodyPos + Number(dv.getBigInt64(b.bufs[1], true)) // the first ts
  assert.equal(dv.getBigInt64(at, true), 1_756_400_000n, 'the fixture ts is where the walk says')
  refused(setU64(small, at, 1n << 62n), /cannot be read exactly/, 'a timestamp of 2^62')
  refused(setU64(small, at, (1n << 53n) + 1n), /cannot be read exactly/, 'a timestamp just past 2^53')
  assert.equal(decodeSeries(setU64(small, at, 1n << 52n)).ts[0], 2 ** 52, 'one that fits is read as it is')
})

test('list offsets that decrease, start past zero, or run past their values are refused', () => {
  const small = load('small.arrow')
  const b = layout(small)[1]
  const dv = new DataView(small.buffer, small.byteOffset, small.byteLength)
  // samples: nodes 4 (list) and 5 (values); buffers 8 (validity), 9 (offsets), 10, 11 (values).
  const offs = b.bodyPos + Number(dv.getBigInt64(b.bufs[9], true))
  const at = (i: number) => offs + 4 * i
  const current = Array.from({ length: 8 }, (_, i) => dv.getInt32(at(i), true))
  assert.deepEqual(current, [0, 0, 1, 3, 6, 6, 7, 9], 'the fixture offsets are where the walk says')
  refused(setI32(small, at(3), 0), /decrease or are negative/, 'a decreasing offset')
  refused(setI32(small, at(2), -1), /decrease or are negative/, 'a negative offset')
  refused(setI32(small, at(4), 0x7fffffff), /decrease or are negative|past their values/, 'an offset of two billion')
  refused(setI32(small, at(7), 1000), /past their values/, 'the last offset past the values')
  let shifted = small
  for (let i = 0; i < 8; i++) shifted = setI32(shifted, at(i), current[i] + 1)
  refused(shifted, /do not start at zero/, 'offsets that all start at one')
  refused(setU64(small, b.bufs[9] + 8, 4n), /offsets buffer shorter/, 'an offsets buffer too short for its rows')
  // The extra series are lists too, read after the same checks.
  const sOffs = b.bodyPos + Number(dv.getBigInt64(b.bufs[17], true))
  refused(setI32(small, sOffs + 4 * 7, 0x7fffffff), /past their values/, 'an extra series with offsets past its values')
})

// ---- one corrupt byte, and several --------------------------------------------

const VALUES = (b: number) => [0x00, 0xff, 0x7f, 0x80, b ^ 0x01, b ^ 0x80]

for (const name of NAMES) {
  test(`${name}: corrupting any single byte gives a coherent series or a deliberate refusal, quickly`, () => {
    const good = load(`${name}.arrow`)
    const want = expected(name)
    const started = Date.now()
    const seen = { decoded: 0, refused: 0 }
    assert.equal(outcome(good, 'uncorrupted'), 'decoded')
    for (let i = 0; i < good.length; i++) {
      for (const v of VALUES(good[i])) {
        const bad = good.slice()
        bad[i] = v
        const what = `${name}: byte ${i} set to ${v}`
        const result = outcome(bad, what)
        seen[result]++
        if (result === 'decoded') {
          // One byte cannot add or remove an interval: the batch, the nodes and
          // the buffers have to agree about how many there are.
          assert.equal(decodeSeries(bad).ts.length, want.ts.length, `${what} changed the number of rows`)
        }
      }
    }
    assert.ok(seen.refused > 0, `${name}: nothing was refused`)
    assert.ok(Date.now() - started < 20_000, 'decoding corrupted input took too long')
  })
}

// The attack that got past a screen placed in front of a library: bend a header
// so one parser skips it, then corrupt a second place the screen never looks at.
// There is one parser now; this holds it to that.
for (const name of NAMES) {
  test(`${name}: a header bent first, then any second byte, still gives a coherent series or a refusal`, () => {
    const good = load(`${name}.arrow`)
    for (const m of layout(good)) {
      for (const first of [
        [[m.vtablePos + 1, 0x80]], // a vtable size of 0x8000 and up
        [[m.vtablePos, 0xff], [m.vtablePos + 1, 0xff]],
        [[m.pos + 7, 0x80]], // a negative header length
        [[m.pos + 4, 0xff]],
      ] as [number, number][][]) {
        const base = patched(good, first)
        outcome(base, `${name}: ${JSON.stringify(first)}`)
        for (let i = 0; i < base.length; i++) {
          for (const v of [0x00, 0xff, 0x7f, base[i] ^ 0x80]) {
            const bad = base.slice()
            bad[i] = v
            outcome(bad, `${name}: ${JSON.stringify(first)} then byte ${i} = ${v}`)
          }
        }
      }
    }
  })
}

test('integers of every awkward size written into every place give a coherent series or a refusal', () => {
  for (const name of NAMES) {
    const good = load(`${name}.arrow`)
    for (let i = 0; i + 8 <= good.length; i++) {
      for (const v of [0x7fff, 0x8000, 0xffff, 0x7fffffff, 0x80000000, 0xffffffff]) outcome(setU32(good, i, v), `${name}: u32 ${v} at ${i}`)
      for (const v of [1n << 31n, 1n << 32n, 1n << 53n, (1n << 63n) - 1n, 1n << 63n, (1n << 64n) - 1n]) outcome(setU64(good, i, v), `${name}: u64 ${v} at ${i}`)
    }
  }
})

test('random corruption of several bytes gives a coherent series or a refusal', () => {
  const cases = Number(process.env.FUZZ_CASES ?? 5000) // raise it to hunt
  for (const name of NAMES) {
    const good = load(`${name}.arrow`)
    let state = 12345 // a fixed sequence, so a failure can be reproduced
    const next = (n: number) => {
      state = (Math.imul(state, 1103515245) + 12345) >>> 0
      return state % n
    }
    for (let k = 0; k < cases; k++) {
      const bad = good.slice()
      const edits: string[] = []
      for (let j = 1 + next(6); j > 0; j--) {
        const at = next(bad.length)
        const v = next(256)
        bad[at] = v
        edits.push(`${at}=${v}`)
      }
      outcome(bad, `${name} case ${k}: ${edits.join(' ')}`)
    }
  }
})

test('the most rows a response may carry is the most the server will send, with room', () => {
  assert.ok(MAX_ROWS >= 200_000 * 2 && MAX_ROWS <= 5_000_000)
})
