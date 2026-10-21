package sessionstats

// Batch 2 clients that reuse the Pi record format (see pi.rs upstream):
// Oh My Pi, Kimchi, Senpi and Craft Agent each keep the pi JSONL shape and
// differ only in scan root, client id and (for Kimchi) the dedup namespace,
// and (for Craft Agent) one provider rewrite.

// ompParser reads ~/.omp/agent/sessions — one level deeper than pi's own
// root, with subagent and advisor transcripts as sibling files inside a
// same-named directory; a recursive *.jsonl walk picks every complete
// session file up independently, exactly like pi's subagent files.
type ompParser struct{}

func (ompParser) Client() string { return "omp" }

func (ompParser) Roots(home string) []string { return []string{home + "/.omp/agent/sessions"} }

func (ompParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (ompParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	return piParser{client: "omp", roots: ompParser{}.Roots}.ParseFile(file, mtimeMS, emit)
}

// kimchiParser reads ~/.config/kimchi/harness with session-scoped dedup keys.
type kimchiParser struct{}

func (kimchiParser) Client() string { return "kimchi" }

func (kimchiParser) Roots(home string) []string {
	return []string{home + "/.config/kimchi/harness"}
}

func (kimchiParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (kimchiParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parser := piParser{client: "kimchi", roots: kimchiParser{}.Roots, scopedKeys: true}
	return parser.ParseFile(file, mtimeMS, emit)
}

// senpiParser reads ~/.senpi/agent (session dirs one level under it). The
// upstream additionally follows OmO task children roots; that is not ported
// (recorded deviation §12) — single-machine local scans see the children in
// their own root when senpi wrote them there.
type senpiParser struct{}

func (senpiParser) Client() string { return "senpi" }

func (senpiParser) Roots(home string) []string {
	return []string{home + "/.senpi/agent", home + "/.senpi/agent/sessions"}
}

func (senpiParser) Discover(root string) ([]string, error) { return WalkJSONLFiles(root) }

func (senpiParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	return piParser{client: "senpi", roots: senpiParser{}.Roots}.ParseFile(file, mtimeMS, emit)
}

// craftAgentParser reads ~/.craft-agent/workspaces/<ws>/sessions/<id>/.pi-sessions/*.jsonl.
// Only files whose parent directory is exactly `.pi-sessions` are pi-format;
// the rest of a Craft workspace is not known to parse and is excluded so it
// cannot be counted on top of the real transcript.
type craftAgentParser struct{}

func (craftAgentParser) Client() string { return "craft-agent" }

func (craftAgentParser) Roots(home string) []string {
	return []string{home + "/.craft-agent/workspaces"}
}

func (craftAgentParser) Discover(root string) ([]string, error) {
	files, err := WalkJSONLFiles(root)
	if err != nil {
		return nil, err
	}
	kept := files[:0]
	for _, file := range files {
		if dirBase(fileDir(file)) == ".pi-sessions" {
			kept = append(kept, file)
		}
	}
	return kept, nil
}

func (craftAgentParser) ParseFile(file string, mtimeMS int64, emit func(UnifiedEntry)) error {
	parser := piParser{
		client: "craft-agent", roots: craftAgentParser{}.Roots,
		rewriteCustomEndpoint: true,
	}
	return parser.ParseFile(file, mtimeMS, emit)
}
