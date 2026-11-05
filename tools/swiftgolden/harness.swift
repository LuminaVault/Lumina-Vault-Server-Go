import Foundation

// Runs Swift's Indexing sources, unmodified, over JSON cases. Built and run by
// run.sh in a Linux Swift image: Foundation's string behaviour differs on Apple
// platforms, and Linux is what production runs.
//
// The file is renamed to main.swift in the build directory by run.sh.
struct Case: Decodable {
    let op: String
    let s: String?
    let max: Int?
    let ov: Int?
    let t: String?
    let d: String?
    let o: Int?
    let h: String?
    let links: [String]?
    let cands: [String]?
}

struct ChunkOut: Encodable {
    let ordinal: Int
    let headingPath: [String]
    let startLine: Int
    let endLine: Int
    let text: String
    let sha: String
    let chunkID: String
}

struct LinkOut: Encodable {
    let line: Int
    let rawTarget: String
    let targetSlug: String
    let targetHeading: String?
    let label: String?
}

struct ResOut: Encodable {
    let state: String
    let target: Int?
}

struct HeadOut: Encodable {
    let ordinal: Int
    let level: Int
    let text: String
    let line: Int
    let path: [String]
}

struct Out: Encodable {
    var chunks: [ChunkOut]?
    var links: [LinkOut]?
    var str: String?
    var res: [ResOut]?
    var headings: [HeadOut]?
    var ints: [Int]?
    var strs: [String]?
}

func uuidFor(_ i: Int) -> UUID {
    UUID(uuidString: String(format: "00000000-0000-0000-0000-%012x", i + 1))!
}

let tenantDefault = "11111111-2222-3333-4444-555555555555"
let cases = try JSONDecoder().decode([Case].self, from: Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1])))
var outs: [Out] = []
for c in cases {
    var o = Out()
    switch c.op {
    case "chunk":
        let chunks = c.max != nil
            ? MarkdownChunker.chunk(c.s!, maxChars: c.max!, overlapChars: c.ov!)
            : MarkdownChunker.chunk(c.s!)
        let doc = ChunkIDs.documentID(tenantID: UUID(uuidString: c.t ?? tenantDefault)!, path: "golden.md")
        o.chunks = chunks.map {
            ChunkOut(
                ordinal: $0.ordinal, headingPath: $0.headingPath, startLine: $0.startLine, endLine: $0.endLine,
                text: $0.text, sha: $0.contentSHA256,
                chunkID: ChunkIDs.chunkID(documentID: doc, ordinal: $0.ordinal, contentSHA256: $0.contentSHA256)
            )
        }
    case "links":
        o.links = Wikilinks.extract(from: c.s!).map {
            LinkOut(line: $0.line, rawTarget: $0.rawTarget, targetSlug: $0.targetSlug, targetHeading: $0.targetHeading, label: $0.label)
        }
    case "headings":
        let hs = MarkdownHeadings.extract(from: c.s!.components(separatedBy: "\n"))
        let paths = MarkdownHeadings.hierarchy(for: hs)
        o.headings = hs.map { HeadOut(ordinal: $0.ordinal, level: $0.level, text: $0.text, line: $0.line, path: paths[$0.ordinal]!) }
    case "docid":
        o.str = ChunkIDs.documentID(tenantID: UUID(uuidString: c.t!)!, path: c.s!)
    case "docid-memory":
        // DocumentChunkIndexer's path for a memory with no file: `memoryID.uuidString`.
        o.str = ChunkIDs.documentID(tenantID: UUID(uuidString: c.t!)!, path: UUID(uuidString: c.s!)!.uuidString)
    case "chunkid":
        o.str = ChunkIDs.chunkID(documentID: c.d!, ordinal: c.o!, contentSHA256: c.h!)
    case "norm":
        o.str = ChunkIDs.normalize(c.s!)
    case "slug":
        o.str = Wikilinks.normalizeSlug(c.s!)
    case "stem":
        o.str = WikilinkResolver.stem(of: c.s!)
    case "sha":
        o.str = MarkdownChunker.sha256Hex(c.s!)
    case "count":
        o.ints = [c.s!.count]
        o.strs = c.s!.map { String($0) }
    case "lines":
        o.strs = c.s!.components(separatedBy: "\n")
    case "trim":
        o.str = c.s!.trimmingCharacters(in: .whitespaces)
    case "resolve":
        let cands = c.cands!.enumerated().map { WikilinkTarget(vaultFileID: uuidFor($0.offset), path: $0.element) }
        let links = c.links!.compactMap { Wikilinks.parse($0, line: 1) }
        o.res = WikilinkResolver.resolve(links: links, candidates: cands).map { r in
            ResOut(state: r.state.rawValue, target: r.targetVaultFileID.flatMap { id in cands.firstIndex { $0.vaultFileID == id } })
        }
    default:
        fatalError(c.op)
    }
    outs.append(o)
}

let enc = JSONEncoder()
enc.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
_ = FileManager.default.createFile(atPath: CommandLine.arguments[2], contents: try enc.encode(outs))
