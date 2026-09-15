// Clock preload, third form: one offset moves Date and the fs time surface together, so a
// kernel-written mtime and Date.now() stay comparable. A backward Date pin alone leaves every
// kernel mtime in the future of the pinned clock, which the lock store reads as "never
// silent" (measured: the stalled-writer test hangs under pin.mjs).
import fs from 'node:fs'
import fsp from 'node:fs/promises'
import { syncBuiltinESMExports } from 'node:module'

const offset = Number(process.env.FOLIOT_CLOCK_OFFSET_MS) || 0

if (offset) {
  const Real = Date
  const realNow = Real.now
  // A plain constructor returning a real Date, not a subclass: a subclass with rest arguments
  // costs an allocation per call, and a 100,000-record parse showed it as +60 MiB of peak RSS.
  function Pinned(a, b, c, d, e, f, g) {
    if (!new.target) return Real()
    switch (arguments.length) {
      case 0: return new Real(realNow() + offset)
      case 1: return new Real(a)
      case 2: return new Real(a, b)
      case 3: return new Real(a, b, c)
      case 4: return new Real(a, b, c, d)
      case 5: return new Real(a, b, c, d, e)
      case 6: return new Real(a, b, c, d, e, f)
      default: return new Real(a, b, c, d, e, f, g)
    }
  }
  Pinned.prototype = Real.prototype
  Object.setPrototypeOf(Pinned, Real)
  Pinned.now = () => realNow() + offset
  Pinned.parse = Real.parse
  Pinned.UTC = Real.UTC
  Object.defineProperty(Pinned, 'name', { value: 'Date' })
  globalThis.Date = Pinned

  // Stats read from the kernel move forward by the offset; a time handed to utimes moves back.
  const keys = ['atime', 'mtime', 'ctime', 'birthtime']
  const shiftStats = (s) => {
    if (!s || typeof s !== 'object' || s.mtimeMs === undefined) return s
    for (const k of keys) {
      if (typeof s[k + 'Ms'] === 'bigint') {
        s[k + 'Ms'] += BigInt(offset)
        if (s[k + 'Ns'] !== undefined) s[k + 'Ns'] += BigInt(offset) * 1000000n
        s[k] = new Real(Number(s[k + 'Ms']))
      } else if (typeof s[k + 'Ms'] === 'number') {
        s[k + 'Ms'] += offset
        s[k] = new Real(s[k + 'Ms'])
      }
    }
    return s
  }
  const toReal = (t) => {
    if (t instanceof Real) return new Real(t.getTime() - offset)
    if (typeof t === 'number') return t - offset / 1000
    if (typeof t === 'string' && t !== '' && !Number.isNaN(Number(t))) return Number(t) - offset / 1000
    return t
  }
  for (const name of ['statSync', 'lstatSync', 'fstatSync']) {
    const orig = fs[name]
    fs[name] = function (...a) { return shiftStats(orig.apply(this, a)) }
  }
  for (const name of ['stat', 'lstat', 'fstat']) {
    const orig = fs[name]
    fs[name] = function (...a) {
      const cb = a[a.length - 1]
      if (typeof cb === 'function') a[a.length - 1] = (err, s) => cb(err, shiftStats(s))
      return orig.apply(this, a)
    }
    const origP = fsp[name]
    if (origP) fsp[name] = async function (...a) { return shiftStats(await origP.apply(this, a)) }
  }
  for (const name of ['utimesSync', 'lutimesSync', 'futimesSync']) {
    const orig = fs[name]
    fs[name] = function (p, at, mt, ...rest) { return orig.call(this, p, toReal(at), toReal(mt), ...rest) }
  }
  for (const name of ['utimes', 'lutimes', 'futimes']) {
    const orig = fs[name]
    fs[name] = function (p, at, mt, ...rest) { return orig.call(this, p, toReal(at), toReal(mt), ...rest) }
    const origP = fsp[name]
    if (origP) fsp[name] = function (p, at, mt, ...rest) { return origP.call(this, p, toReal(at), toReal(mt), ...rest) }
  }
  syncBuiltinESMExports()
}
