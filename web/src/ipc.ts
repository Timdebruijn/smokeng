// A screen for the Arrow IPC stream a measurements request answers with, run
// before the library reads it.
//
// flechette sizes what it builds from counts it reads out of the stream's
// flatbuffer headers (how many fields, how many children, how many buffers)
// and does not compare them with how many bytes there are. One corrupted byte
// in a header is enough: setting a single byte to 5 in a 700-byte response made
// decoding fill a 4 GB heap and abort the process, which in a browser is a
// dead tab, not an error message.
//
// The server writes exactly one thing: a schema of integer, timestamp and list
// columns, then record batches. So the screen is deliberately blunt. It refuses
// a schema that is not that or that expands to more than it should, and checks
// every count it reads against the bytes that are actually there, with every
// read bounds-checked, before flechette sees the stream. It reads the headers
// itself because the library keeps them private.
//
// What it leaves to flechette, because flechette already refuses it and no test
// can tell the difference: a header or body longer than the response, a
// compressed batch (no codec is registered), and a dictionary (the message type
// is refused here). The offsets inside a list column, which sit in the body, are
// checked in api.ts, where the columns are.
//
// The checks on a record batch's own counts (rows, nodes, buffers, vector sizes)
// overlap: with any one of them removed the tests still pass, and with all of
// them removed random corruption of the response exhausts the heap. They are
// kept together, and the corruption tests in decode.test.ts are what holds them.

const fail = (what: string): never => {
  throw new Error(`measurements: malformed response (${what})`)
}

/** The most fields, children and metadata entries one schema may declare. */
export const MAX_SCHEMA_NODES = 256
const MAX_SCHEMA_DEPTH = 4
const MAX_MESSAGES = 1024

// Arrow's Type union ids: the only column types the server writes.
const TYPE_INT = 2
const TYPE_TIMESTAMP = 10
const TYPE_LIST = 12
// Message header types.
const MSG_SCHEMA = 1
const MSG_RECORD_BATCH = 3
const MAX_BATCH_NODES = 1024
const MAX_BATCH_BUFFERS = 4096

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
  // Written as pos > length - n, not pos + n > length: the sum of two values
  // read from a header is the thing that can be made to wrap or lose precision.
  private ok(pos: number, n: number) {
    return Number.isInteger(pos) && pos >= 0 && pos <= this.buf.length - n
  }
  u8(pos: number) {
    return this.ok(pos, 1) ? this.buf[pos] : fail('read past the end')
  }
  u16(pos: number) {
    return this.ok(pos, 2) ? this.view.getUint16(pos, true) : fail('read past the end')
  }
  u32(pos: number) {
    return this.ok(pos, 4) ? this.view.getUint32(pos, true) : fail('read past the end')
  }
  i32(pos: number) {
    return this.ok(pos, 4) ? this.view.getInt32(pos, true) : fail('read past the end')
  }
  /** An unsigned 64-bit value, refused if a double cannot hold it exactly. */
  u64(pos: number) {
    if (!this.ok(pos, 8)) return fail('read past the end')
    const v = this.view.getBigUint64(pos, true)
    return v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : fail('a length that cannot be real')
  }
}

/** A flatbuffer table at an absolute position. */
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
    if (size < 4) return fail('a vtable too small to be one')
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
    if (rel > this.b.length - p || to >= this.b.length) return fail('an offset outside the header')
    return new Table(this.b, to)
  }
  /** Follow an offset field to a vector: where its elements start and how many. */
  vector(vt: number, elemSize: number): { first: number; n: number } {
    const p = this.field(vt)
    if (p < 0) return { first: 0, n: 0 }
    const rel = this.b.u32(p)
    if (rel > this.b.length - p) return fail('an offset outside the header')
    const vec = p + rel
    const n = this.b.u32(vec)
    // The elements have to be there: a count larger than the bytes left is the
    // lie that makes a reader loop or allocate for something that does not exist.
    if (n > Math.floor((this.b.length - vec - 4) / elemSize)) return fail('a vector longer than the header')
    return { first: vec + 4, n }
  }
  element(first: number, i: number): Table {
    const at = first + 4 * i
    const rel = this.b.u32(at)
    if (rel > this.b.length - at - 1) return fail('an offset outside the header')
    return new Table(this.b, at + rel)
  }
}

function root(meta: Bytes): Table {
  const off = meta.u32(0)
  return off < meta.length ? new Table(meta, off) : fail('a root outside the header')
}

/**
 * Spend one unit of budget for every field and metadata entry a schema, or a
 * field, names, each time it names it. A reference is counted before it is
 * followed, so the walk does no more work than the budget whatever the buffer
 * says: sharing a child among a million parents costs a million units, the same
 * as a reader that expands them.
 */
function countNodes(t: Table, budget: { left: number }, depth: number, isSchema: boolean) {
  if (depth > MAX_SCHEMA_DEPTH) fail('fields nested too deep')
  const spend = (n: number) => {
    if ((budget.left -= n) < 0) fail(`more than ${MAX_SCHEMA_NODES} fields`)
  }
  spend(t.vector(isSchema ? 8 : 16, 4).n) // custom_metadata
  const kids = t.vector(isSchema ? 6 : 14, 4) // fields, or children
  spend(kids.n)
  for (let i = 0; i < kids.n; i++) {
    const field = t.element(kids.first, i)
    const typeAt = field.field(8)
    const type = typeAt < 0 ? 0 : t.b.u8(typeAt)
    if (type !== TYPE_INT && type !== TYPE_TIMESTAMP && type !== TYPE_LIST) fail(`a column of type ${type}`)
    countNodes(field, budget, depth + 1, false)
  }
}

/**
 * Refuse a stream the decoder should not be given. Throws an Error beginning
 * "measurements:"; returns normally for what the server writes.
 */
export function screenIPC(bytes: Uint8Array): void {
  const b = new Bytes(bytes)
  let pos = 0
  let messages = 0
  while (pos < b.length) {
    let len = b.u32(pos)
    pos += 4
    if (len === 0xffffffff) {
      len = b.u32(pos) // continuation marker, then the real length
      pos += 4
    }
    if (len === 0) return // end of stream
    if (++messages > MAX_MESSAGES) fail(`more than ${MAX_MESSAGES} messages`)
    const meta = new Bytes(bytes.subarray(pos, pos + len))
    pos += len
    const msg = root(meta)
    const typeAt = msg.field(6)
    const type = typeAt < 0 ? 0 : meta.u8(typeAt)
    const bodyAt = msg.field(10)
    const bodyLen = bodyAt < 0 ? 0 : meta.u64(bodyAt)
    if (type === MSG_SCHEMA) {
      if (messages !== 1) fail('a schema that is not first')
      const schema = msg.table(8)
      if (schema) countNodes(schema, { left: MAX_SCHEMA_NODES }, 0, true)
    } else if (type === MSG_RECORD_BATCH) {
      // What the batch says about its own buffers has to fit the body. Each
      // one is checked, and the number of them is bounded first, because a
      // count read from the header is what the loop would otherwise run for.
      const batch = msg.table(8)
      const rows = batch && batch.field(4) >= 0 ? meta.u64(batch.field(4)) : 0
      if (rows > b.length) fail('more rows than bytes') // a row costs bytes
      const nodes = batch ? batch.vector(6, 16) : { first: 0, n: 0 }
      if (nodes.n > MAX_BATCH_NODES) fail('too many column nodes')
      for (let i = 0; i < nodes.n; i++) {
        if (meta.u64(nodes.first + 16 * i) > b.length) fail('a column longer than the response')
      }
      const bufs = batch ? batch.vector(8, 16) : { first: 0, n: 0 }
      if (bufs.n > MAX_BATCH_BUFFERS) fail('too many buffers')
      for (let i = 0; i < bufs.n; i++) {
        const off = meta.u64(bufs.first + 16 * i)
        const size = meta.u64(bufs.first + 16 * i + 8)
        if (off > bodyLen || size > bodyLen - off) fail('a buffer outside the body')
      }
    } else {
      fail(`a message of type ${type}`)
    }
    pos += bodyLen
  }
}
