import Foundation
// Derives the stdlib's grapheme break data by observing String.count on the
// toolchain production runs. Output: one line per run of scalars with the same
// class: "lo hi class pict consonant whitespace".
//
//   swiftc -O probe.swift -o probe && ./probe runs.txt backslash.txt
func n(_ scalars: [UInt32]) -> Int {
  var v = String.UnicodeScalarView()
  for s in scalars { v.append(Unicode.Scalar(s)!) }
  return String(v).count
}
let ZWJ: UInt32 = 0x200D, EM: UInt32 = 0x1F600, A: UInt32 = 0x61, COMB: UInt32 = 0x301
let L: UInt32 = 0x1100, V: UInt32 = 0x1161, T: UInt32 = 0x11A8, KA: UInt32 = 0x915, VIRAMA: UInt32 = 0x94D, RI: UInt32 = 0x1F1E6
func classify(_ c: UInt32) -> String {
  if c == 0x0D { return "cr" }
  if c == 0x0A { return "lf" }
  if n([c, COMB]) == 2 { return "control" }
  if c == ZWJ { return "zwj" }
  if n([A, c]) == 1 { return n([EM, c, ZWJ, EM]) == 1 ? "extend" : "spacing" }
  if n([c, A]) == 1 { return "prepend" }
  if n([c, L]) == 1 { return "l" }
  if n([c, V]) == 1 { return n([V, c]) == 1 ? "v" : "lv" }
  if n([c, T]) == 1 { return n([L, c]) == 1 ? "lvt" : "t" }
  if n([RI, c]) == 1 { return "ri" }
  return "any"
}
var out = ""
var runLo: UInt32 = 0; var runKey = ""; var started = false
var eqs: [String] = []
let ascii = (0x20...0x7E).map { Character(Unicode.Scalar($0)!) }
func emit(_ lo: UInt32, _ hi: UInt32, _ key: String) { out += "\(String(lo, radix: 16)) \(String(hi, radix: 16)) \(key)\n" }
let ws = CharacterSet.whitespaces
var last: UInt32 = 0
for c in UInt32(0)...0x10FFFF {
  if c >= 0xD800 && c <= 0xDFFF { continue }
  let cl = classify(c)
  let pict = n([c, ZWJ, EM]) == 1 ? 1 : 0
  let cons = (cl == "any" || cl == "l" || cl == "v" || cl == "t" || cl == "lv" || cl == "lvt") && n([c, VIRAMA, KA]) == 1 ? 1 : 0
  let u = Unicode.Scalar(c)!
  let w = ws.contains(u) ? 1 : 0
  let key = "\(cl) \(pict) \(cons) \(w)"
  if !started { runLo = c; runKey = key; started = true }
  else if key != runKey || (c == 0xE000) { emit(runLo, last, runKey); runLo = c; runKey = key }
  last = c
  let ch = Character(u)
  if c > 0x7F { for a in ascii where a == ch { eqs.append("\(String(c, radix: 16))=\(a)") } }
}
emit(runLo, last, runKey)
try out.write(toFile: CommandLine.arguments[1], atomically: true, encoding: .utf8)

// replacingOccurrences(of: "\\", with: "/") goes through CoreFoundation's search,
// which refuses a match followed by a scalar in CF's own combining table. That table
// is neither Characters nor a current Unicode, so read it. One line per run of
// scalars that keep the backslash.
var keep = ""
var keepLo: UInt32?
var keepLast: UInt32 = 0
for c in UInt32(0)...0x10FFFF {
  if c >= 0xD800 && c <= 0xDFFF { continue }
  var v = String.UnicodeScalarView()
  v.append("\\"); v.append(Unicode.Scalar(c)!); v.append("x")
  let kept = String(v).replacingOccurrences(of: "\\", with: "/").unicodeScalars.first == "\\"
  if kept, keepLo == nil { keepLo = c }
  if !kept, let lo = keepLo { keep += "\(String(lo, radix: 16)) \(String(keepLast, radix: 16))\n"; keepLo = nil }
  keepLast = c
}
if let lo = keepLo { keep += "\(String(lo, radix: 16)) \(String(keepLast, radix: 16))\n" }
try keep.write(toFile: CommandLine.arguments[2], atomically: true, encoding: .utf8)
print("canonical-equal-to-ascii:", eqs)
print("runs:", out.split(separator: "\n").count)
