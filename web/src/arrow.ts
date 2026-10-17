// A reader for the Arrow IPC stream the measurements endpoint writes, and for
// nothing else.
//
// It replaces a general library on purpose. flechette sizes what it builds from
// counts in the stream's headers and from offsets in its body without comparing
// them with the bytes that are there, so one corrupt byte could fill a 4 GB heap
// (a dead tab), or give rows that were never sent: a column that claims more rows
// than it holds reads as NaN, and a NaN loss reads as "0.0%", a confident number
// for an interval nobody measured. A screen placed in front of the library was
// tried, and it kept finding the same thing: two parsers that disagree about a
// header (flechette reads some sizes signed, the format says unsigned) leave a
// gap between what was checked and what was read. This reads the stream once,
// with one set of rules, and every length it takes from a header is compared with
// the bytes it describes before anything is allocated from it.
//
// The stream is the one writeMeasurementsArrow writes (internal/api/
// measurements.go): a schema of integer, timestamp and list-of-integer columns,
// record batches, and an end-of-stream marker. Anything else is an error, a
// stream without its end marker included: a response cut short must not read as
// a shorter series. The reader assumes little-endian, as every platform the
// server is released for is.
//
// What it guarantees is the shape: the rows the response holds are the rows the
// result has, nothing is read from outside the response, and no number in the
// result is made of nothing. What it cannot do is notice a changed value. There
// is no checksum, and a flipped bit in a sample is a different sample.

import type { ExtraSeries, Series } from './api.ts'

const fail = (what: string): never => {
  throw new Error(`measurements: malformed response (${what})`)
}

/** The most rows one response may carry; the server returns at most 200,000. */
export const MAX_ROWS = 1_000_000
export const MAX_FIELDS = 64
export const MAX_MESSAGES = 4096

// Arrow's Type union ids for the column types the server writes, and the
// message header types.
const T_INT = 2
const T_TIMESTAMP = 10
const T_LIST = 12
const MSG_SCHEMA = 1
const MSG_RECORD_BATCH = 3

// No parameter properties: the tests load these files with Node's type
// stripping, which erases types and nothing else.
class Bytes {
  readonly buf: Uint8Array
  readonly view: DataView
  constructor(buf: Uint8Array) {
    this.buf = buf
    this.view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength)
  }
  get length() {
    return this.buf.length
  }
  // Written as pos > length - n, never pos + n > length: a sum of two values read
  // from a header is what can wrap or lose precision.
  private ok(pos: number, n: number) {
    return Number.isInteger(pos) && pos >= 0 && pos <= this.buf.length - n
  }
  u8(pos: number) {
    return this.ok(pos, 1) ? this.buf[pos] : fail('a read past the end')
  }
  u16(pos: number) {
    return this.ok(pos, 2) ? this.view.getUint16(pos, true) : fail('a read past the end')
  }
  i16(pos: number) {
    return this.ok(pos, 2) ? this.view.getInt16(pos, true) : fail('a read past the end')
  }
  u32(pos: number) {
    return this.ok(pos, 4) ? this.view.getUint32(pos, true) : fail('a read past the end')
  }
  i32(pos: number) {
    return this.ok(pos, 4) ? this.view.getInt32(pos, true) : fail('a read past the end')
  }
  /** A 64-bit integer that must fit a double exactly, and not be negative. */
  u64(pos: number) {
    if (!this.ok(pos, 8)) return fail('a read past the end')
    const v = this.view.getBigInt64(pos, true)
    return v >= 0n && v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : fail('a length that cannot be real')
  }
}

/** A flatbuffer table. Field offsets are unsigned 16-bit, as the format says. */
class Table {
  readonly b: Bytes
  readonly pos: number
  constructor(b: Bytes, pos: number) {
    this.b = b
    this.pos = pos
  }
  /** Where the field at this vtable offset is, or -1 when it is absent. */
  field(vt: number): number {
    const vtable = this.pos - this.b.i32(this.pos)
    const size = this.b.u16(vtable)
    if (size < 4) return fail('a vtable that is not one')
    if (vt >= size) return -1 // how a flatbuffer says "default"
    const rel = this.b.u16(vtable + vt)
    if (rel === 0) return -1
    const at = this.pos + rel
    return at < this.b.length ? at : fail('a field outside the header')
  }
  /** Follow an offset field to a table. */
  table(vt: number): Table | null {
    const p = this.field(vt)
    if (p < 0) return null
    const rel = this.b.u32(p)
    const to = p + rel
    return to < this.b.length ? new Table(this.b, to) : fail('an offset outside the header')
  }
  /** Follow an offset field to a vector: where its elements start, and how many. */
  vector(vt: number): { first: number; n: number } {
    const p = this.field(vt)
    if (p < 0) return { first: 0, n: 0 }
    const vec = p + this.b.u32(p)
    return { first: vec + 4, n: this.b.u32(vec) } // the callers bound n before they use it
  }
  /** The i'th element of a vector of tables. */
  element(first: number, i: number): Table {
    const at = first + 4 * i
    return new Table(this.b, at + this.b.u32(at))
  }
  string(vt: number): string {
    const p = this.field(vt)
    if (p < 0) return ''
    const at = p + this.b.u32(p)
    const n = this.b.u32(at)
    return new TextDecoder().decode(this.b.buf.subarray(at + 4, at + 4 + n))
  }
  u8(vt: number, dflt: number): number {
    const p = this.field(vt)
    return p < 0 ? dflt : this.b.u8(p)
  }
  i16(vt: number, dflt: number): number {
    const p = this.field(vt)
    return p < 0 ? dflt : this.b.i16(p)
  }
  i32(vt: number, dflt: number): number {
    const p = this.field(vt)
    return p < 0 ? dflt : this.b.i32(p)
  }
  u64(vt: number): number {
    const p = this.field(vt)
    return p < 0 ? 0 : this.b.u64(p)
  }
}

interface IntSpec {
  bytes: 1 | 2 | 4
  signed: boolean
}
type ColSpec =
  | { name: string; kind: 'int'; int: IntSpec }
  | { name: string; kind: 'timestamp'; unit: number }
  | { name: string; kind: 'list'; int: IntSpec }

/** An integer of 8, 16 or 32 bits: what fits a typed array without changing. */
function intSpec(t: Table): IntSpec {
  const bits = t.i32(4, 0)
  if (bits !== 8 && bits !== 16 && bits !== 32) return fail(`an integer of ${bits} bits`)
  return { bytes: (bits / 8) as 1 | 2 | 4, signed: t.u8(6, 0) !== 0 }
}

function readSchema(schema: Table): ColSpec[] {
  const fields = schema.vector(6)
  if (fields.n === 0 || fields.n > MAX_FIELDS) return fail(`a schema of ${fields.n} fields`)
  const cols: ColSpec[] = []
  for (let i = 0; i < fields.n; i++) {
    const f = schema.element(fields.first, i)
    const name = f.string(4)
    if (f.field(12) >= 0) fail('a dictionary-encoded column')
    const typeId = f.u8(8, 0)
    const type = f.table(10)
    if (cols.some((c) => c.name === name)) fail(`two columns named ${name}`)
    if (typeId === T_INT && type) {
      cols.push({ name, kind: 'int', int: intSpec(type) })
    } else if (typeId === T_TIMESTAMP && type) {
      const unit = type.i16(4, 0)
      if (unit < 0 || unit > 3) fail('a time unit that is not one')
      cols.push({ name, kind: 'timestamp', unit })
    } else if (typeId === T_LIST) {
      const kids = f.vector(14)
      if (kids.n !== 1) fail('a list without exactly one child')
      const child = f.element(kids.first, 0)
      const childType = child.table(10)
      if (child.u8(8, 0) !== T_INT || !childType) fail('a list of something other than integers')
      cols.push({ name, kind: 'list', int: intSpec(childType!) })
    } else {
      fail(`a column of type ${typeId}`)
    }
  }
  return cols
}

/** What one batch holds for one column, as the reader needs it. */
interface Chunk {
  rows: number
  valid: (i: number) => boolean
  int?: (i: number) => number
  offsets?: Int32Array // rows + 1, relative to the child
  child?: (i: number) => number
}

function readInt(body: Uint8Array, view: DataView, off: number, len: number, rows: number, spec: IntSpec) {
  if (rows > Math.floor(len / spec.bytes)) fail('a column shorter than its rows')
  return (i: number): number => {
    const p = off + i * spec.bytes
    switch (spec.bytes) {
      case 1:
        return spec.signed ? view.getInt8(p) : body[p]
      case 2:
        return spec.signed ? view.getInt16(p, true) : view.getUint16(p, true)
      default:
        return spec.signed ? view.getInt32(p, true) : view.getUint32(p, true)
    }
  }
}

const SECONDS_PER_UNIT = [1, 1e3, 1e6, 1e9]

/**
 * Read a measurements response. extraNames are the optional list columns to look
 * for besides the required ones; a column in the stream that is none of them is
 * skipped, but its type has to be one the server writes.
 */
export function readMeasurements(bytes: Uint8Array, extraNames: readonly string[]): Series {
  const b = new Bytes(bytes)
  let pos = 0
  let messages = 0
  let cols: ColSpec[] | null = null
  let ended = false
  const chunks: Chunk[][] = [] // per batch, per column
  let totalRows = 0

  while (pos < b.length) {
    if (b.u32(pos) !== 0xffffffff) return fail('no continuation marker (not a stream the server writes)')
    const metaLen = b.u32(pos + 4)
    pos += 8
    if (metaLen === 0) {
      ended = true
      if (pos !== b.length) fail('bytes after the end-of-stream marker')
      break
    }
    if (metaLen > b.length - pos) return fail('a header longer than the response')
    if (++messages > MAX_MESSAGES) return fail('too many messages')
    const meta = new Bytes(bytes.subarray(pos, pos + metaLen))
    pos += metaLen
    const msg = new Table(meta, meta.u32(0))
    const type = msg.u8(6, 0)
    const bodyLen = msg.u64(10)
    if (bodyLen > b.length - pos) return fail('a body longer than the response')
    const body = bytes.subarray(pos, pos + bodyLen)
    pos += bodyLen
    const view = new DataView(body.buffer, body.byteOffset, body.byteLength)

    if (type === MSG_SCHEMA) {
      if (messages !== 1) return fail('a schema that is not first')
      const schema = msg.table(8)
      if (!schema) return fail('a schema message without a schema')
      cols = readSchema(schema!)
    } else if (type === MSG_RECORD_BATCH) {
      if (!cols) return fail('a record batch before any schema')
      const batch = msg.table(8)
      if (!batch) return fail('a record batch message without a batch')
      if (batch!.field(10) >= 0) fail('a compressed record batch')
      const rows = batch!.u64(4)
      totalRows += rows
      if (totalRows > MAX_ROWS) return fail('more rows than one response may carry')

      // The nodes and buffers the schema implies, in the order Arrow writes them:
      // a column is one node with a validity and a data buffer, a list is its own
      // node with a validity and an offsets buffer, then its child's.
      const nodes = batch!.vector(6)
      const bufs = batch!.vector(8)
      const wantNodes = cols.reduce((n, c) => n + (c.kind === 'list' ? 2 : 1), 0)
      if (nodes.n !== wantNodes || bufs.n !== 2 * wantNodes) fail('a batch that does not match the schema')
      const node = (i: number) => ({
        len: meta.u64(nodes.first + 16 * i),
        nulls: meta.u64(nodes.first + 16 * i + 8),
      })
      const buffer = (i: number) => {
        const off = meta.u64(bufs.first + 16 * i)
        const len = meta.u64(bufs.first + 16 * i + 8)
        if (off > body.length || len > body.length - off) fail('a buffer outside the body')
        return { off, len }
      }
      // An empty validity buffer means every value is valid.
      const validity = (i: number, n: number) => {
        const v = buffer(i)
        if (v.len === 0) return () => true
        if (n > v.len * 8) fail('a validity bitmap shorter than its rows')
        return (k: number) => ((body[v.off + (k >> 3)] >> (k & 7)) & 1) === 1
      }

      const chunk: Chunk[] = []
      let ni = 0
      let bi = 0
      for (const c of cols) {
        const n = node(ni)
        if (n.len !== rows) fail('a column whose length is not the batch length')
        if (n.nulls > n.len) fail('more nulls than values')
        // A null count has to be believable: nulls need a bitmap to be in.
        if (n.nulls > 0 && buffer(bi).len === 0) fail('nulls with no validity bitmap')
        const valid = validity(bi, n.len)
        if (c.kind === 'list') {
          const offs = buffer(bi + 1)
          if (n.len + 1 > Math.floor(offs.len / 4)) fail('an offsets buffer shorter than its rows')
          const offsets = new Int32Array(n.len + 1)
          let prev = 0
          for (let i = 0; i <= n.len; i++) {
            const v = view.getInt32(offs.off + 4 * i, true)
            if (v < prev) fail('list offsets that decrease or are negative')
            offsets[i] = prev = v
          }
          if (offsets[0] !== 0) fail('list offsets that do not start at zero') // the server never slices
          const cn = node(ni + 1)
          if (cn.nulls !== 0) fail('a list of nullable values')
          if (prev > cn.len) fail('list offsets past their values')
          buffer(bi + 2) // the child's validity: in bounds, and not consulted, since no value is null
          const data = buffer(bi + 3)
          chunk.push({ rows: n.len, valid, offsets, child: readInt(body, view, data.off, data.len, cn.len, c.int) })
          ni += 2
          bi += 4
        } else {
          const data = buffer(bi + 1)
          const int =
            c.kind === 'timestamp'
              ? readInt64(view, data.off, data.len, n.len)
              : readInt(body, view, data.off, data.len, n.len, c.int)
          chunk.push({ rows: n.len, valid, int })
          ni += 1
          bi += 2
        }
      }
      chunks.push(chunk)
    } else {
      return fail(`a message of type ${type}`)
    }
  }
  if (!cols) return fail('no schema')
  if (!ended) return fail('the response ends before its end-of-stream marker; it was cut short')

  return assemble(cols, chunks, totalRows, extraNames)
}

/** A 64-bit signed integer column, which must be exactly representable. */
function readInt64(view: DataView, off: number, len: number, rows: number) {
  if (rows > Math.floor(len / 8)) fail('a column shorter than its rows')
  return (i: number): number => {
    const n = Number(view.getBigInt64(off + i * 8, true))
    return Number.isSafeInteger(n) ? n : fail('a 64-bit value that cannot be read exactly')
  }
}

function assemble(cols: ColSpec[], chunks: Chunk[][], rows: number, extraNames: readonly string[]): Series {
  const index = (name: string, kind: ColSpec['kind']) => cols.findIndex((c) => c.name === name && c.kind === kind)
  const need = (name: string, kind: ColSpec['kind']) => {
    const i = index(name, kind)
    return i >= 0 ? i : fail(`the response has no ${name} column of the right type`)
  }
  const iTs = need('ts', 'timestamp')
  const iSent = need('sent', 'int')
  const iRecv = need('received', 'int')
  const iFlags = need('flags', 'int')
  const iSamples = need('samples', 'list')
  const iIcmp = index('icmp_error', 'int')
  const iSend = index('send_error', 'int')

  // The types the values are stored in: flags is a byte, so it has to be an
  // unsigned byte; the round-trip samples are unsigned 32-bit and the extra
  // series signed 32-bit. Anything else would change a value on the way in.
  const spec = (i: number) => (cols[i] as { int: IntSpec }).int
  if (spec(iFlags).bytes !== 1 || spec(iFlags).signed) fail('flags that are not an unsigned byte')
  if (spec(iSamples).bytes !== 4 || spec(iSamples).signed) fail('samples that are not unsigned 32-bit')

  const ts = new Float64Array(rows)
  const sent = new Float64Array(rows)
  const received = new Float64Array(rows)
  const flags = new Uint8Array(rows)
  const icmpErrors: (number | null)[] = new Array(rows)
  const sendErrors: (number | null)[] = new Array(rows)
  const perUnit = SECONDS_PER_UNIT[(cols[iTs] as { unit: number }).unit]

  const lists = (col: number, signed: boolean) => {
    const offsets = new Uint32Array(rows + 1)
    let total = 0
    for (const ch of chunks) total += ch[col].offsets![ch[col].rows]
    const values = signed ? new Int32Array(total) : new Uint32Array(total)
    const measured = new Uint8Array(rows)
    let r = 0
    let w = 0
    for (const ch of chunks) {
      const c = ch[col]
      for (let i = 0; i < c.rows; i++, r++) {
        for (let k = c.offsets![i]; k < c.offsets![i + 1]; k++) values[w++] = c.child!(k)
        offsets[r + 1] = w
        measured[r] = c.valid(i) ? 1 : 0
      }
    }
    return { offsets, values, measured }
  }

  // A null in a column every interval has is not a value: read as the zero
  // sitting in its slot, a null `received` would be a hundred percent loss.
  for (const ch of chunks) {
    for (const col of [iTs, iSent, iRecv, iFlags, iSamples]) {
      for (let i = 0; i < ch[col].rows; i++) if (!ch[col].valid(i)) fail(`a null in the ${cols[col].name} column`)
    }
  }

  let r = 0
  for (const ch of chunks) {
    for (let i = 0; i < ch[iTs].rows; i++, r++) {
      ts[r] = ch[iTs].int!(i) / perUnit
      sent[r] = ch[iSent].int!(i)
      received[r] = ch[iRecv].int!(i)
      flags[r] = ch[iFlags].int!(i)
      icmpErrors[r] = iIcmp >= 0 && ch[iIcmp].valid(i) ? ch[iIcmp].int!(i) : null
      sendErrors[r] = iSend >= 0 && ch[iSend].valid(i) ? ch[iSend].int!(i) : null
    }
  }

  const samples = lists(iSamples, false)
  // A list column that is null on every row means the target measures nothing of
  // the sort; it is left out, since an empty graph would invite the reader to
  // conclude the jitter was zero.
  const extra: Record<string, ExtraSeries> = {}
  for (const name of extraNames) {
    const i = index(name, 'list')
    if (i < 0) continue
    if (spec(i).bytes !== 4 || !spec(i).signed) fail(`the ${name} column is not signed 32-bit`)
    const e = lists(i, true)
    if (e.measured.some((m) => m === 1)) extra[name] = { offsets: e.offsets, values: e.values as Int32Array, measured: e.measured }
  }
  return {
    ts, sent, received, flags,
    offsets: samples.offsets, values: samples.values as Uint32Array,
    icmpErrors, sendErrors, extra,
  }
}
