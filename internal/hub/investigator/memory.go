package investigator

import (
	"fmt"
	"strings"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// memoryDigestRecordCap bounds how much of a single memory record's content
// the digest reproduces — the digest is a re-grounding aid, never a dump.
const memoryDigestRecordCap = 600

// RenderMemoryDigest produces a compact, operator/LLM-facing digest of an
// investigation's durable memory records (findings, evidence, ruled-out
// branches, compaction summaries). It is the internal helper Task 8 calls for
// prompt/answer assembly: the Markdown export embeds it today, and
// prompt-cache-aware assembly (Task 15) will fold it into the live context to
// re-ground the model on pre-compaction evidence without reloading raw
// artifacts. Returns "" when there is nothing durable to show.
func RenderMemoryDigest(mems []store.InvestigationMemory) string {
	if len(mems) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range mems {
		fmt.Fprintf(&b, "- `%s` (%s): %s\n",
			m.ID, m.Kind, oneLine(capNotebook(m.Content, memoryDigestRecordCap)))
	}
	return b.String()
}
