// Package reviewqa owns the review conversation's on-disk protocol: the
// questions a review turn emits while it works, and the answers an operator
// writes back.
//
// Two append-only newline-delimited JSON files live in the run's evidence
// directory. Append-only is the durability story: a crash mid-write loses at
// most the trailing line, a reader can tail either file while a writer is
// appending, and neither side needs a lock. The reviewer writes questions with
// the file tools it already has, which is why there is no MCP server here - one
// per run would be a process, a handshake and a per-adapter support matrix for
// a capability the agent already has.
//
// User-facing semantics are owned by
// docs/src/content/docs/concepts/review-conversation.md.
package reviewqa

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kinds a questions.ndjson line can carry.
const (
	KindQuestion = "question"
	KindRetract  = "retract"
)

// Weights a question can carry. Only major questions are ever emitted: the
// reviewer decides minor ones itself (captain's ruling of 2026-09-15, routing
// by weight unchanged), so a minor line is a protocol violation and is
// reported rather than silently escalated.
const (
	WeightMajor = "major"
	WeightMinor = "minor"
)

// File names inside the conversation directory.
const (
	QuestionsFile = "questions.ndjson"
	AnswersFile   = "answers.ndjson"
)

// Bounds on what is read from either file. They exist because every loaded
// entry is rendered into an agent prompt and into `axi` output, so an agent
// that appends in a loop must degrade to a truncated conversation rather than
// an unbounded one.
//
// The two bounds behave oppositely and both matter. maxLines counts accepted
// lines and keeps the NEWEST of them, because a later line supersedes an
// earlier one with the same id. maxFileBytes stops the scan instead, so a file
// over that size keeps its LEADING bytes and a trailing retraction or answer
// may not be read at all.
const (
	maxLines     = 2000
	maxFileBytes = 4 << 20 // 4 MiB
	maxLineBytes = 64 << 10
)

// Question is one line of questions.ndjson. A KindRetract line carries only
// ID, Kind, Reason and At.
type Question struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	Weight   string   `json:"weight,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Area     string   `json:"area,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	AskedAt  string   `json:"asked_at,omitempty"`
	At       string   `json:"at,omitempty"`
}

// Answer is one line of answers.ndjson.
type Answer struct {
	ID         string `json:"id"`
	Answer     string `json:"answer"`
	AnsweredBy string `json:"answered_by,omitempty"`
	AnsweredAt string `json:"answered_at,omitempty"`
}

// Entry is a question resolved against every later line about the same id:
// the newest question wins, a retraction closes it, and the newest answer for
// it is attached.
type Entry struct {
	Question
	Answer    *Answer
	Retracted bool
}

// Open reports whether this entry still blocks the review step: a live
// question with no answer.
func (e Entry) Open() bool { return !e.Retracted && e.Answer == nil }

// Answered reports whether a live question has an answer.
func (e Entry) Answered() bool { return !e.Retracted && e.Answer != nil }

// Conversation is one run's questions in the order they were first asked.
type Conversation struct {
	Entries []Entry
	// Notes carries bounded, operator-readable reasons a line was dropped:
	// malformed JSON, a missing id, a minor-weight question, an answer for an
	// id nobody asked. Never an error - a half-written trailing line is
	// expected while the reviewer is still appending.
	Notes []string
}

// Open returns the entries that still block the step.
func (c Conversation) Open() []Entry { return c.filter(Entry.Open) }

// Answered returns the live, answered entries.
func (c Conversation) Answered() []Entry { return c.filter(Entry.Answered) }

// Withdrawn returns the retracted entries.
func (c Conversation) Withdrawn() []Entry {
	return c.filter(func(e Entry) bool { return e.Retracted })
}

func (c Conversation) filter(keep func(Entry) bool) []Entry {
	var out []Entry
	for _, e := range c.Entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// Dir is the review conversation directory for a run, given that run's
// evidence directory. Empty in, empty out: an embedding with no evidence
// directory has no conversation, and callers treat that as "no questions".
func Dir(evidenceDir string) string {
	if strings.TrimSpace(evidenceDir) == "" {
		return ""
	}
	return filepath.Join(evidenceDir, "review")
}

// Load reads a conversation directory. A missing directory or a missing file
// is an empty conversation, not an error: the reviewer creates the files only
// when it has something to say.
func Load(dir string) (Conversation, error) {
	var conv Conversation
	if strings.TrimSpace(dir) == "" {
		return conv, nil
	}

	questionLines, qTruncated, err := readLines(filepath.Join(dir, QuestionsFile))
	if err != nil {
		return conv, err
	}
	answerLines, aTruncated, err := readLines(filepath.Join(dir, AnswersFile))
	if err != nil {
		return conv, err
	}

	order := make([]string, 0, len(questionLines))
	byID := make(map[string]*Entry, len(questionLines))
	// An id can be ASKED more than once: ids are chosen by the agent (the
	// protocol's worked example is literally "q1"), the conversation directory
	// is per RUN, and a cold rereview in a fix round is shown only the OPEN
	// questions - so it reuses "q1" for a genuinely different question. An
	// answer written before that re-ask answered the OLD question, and letting
	// it settle the new one meant the new question arrived pre-answered:
	// Open() was empty, no finding was emitted, the gate never parked, and a
	// major question reached nobody.
	//
	// So a question is settled only once it has AS MANY answers as it has been
	// asked. Counting is deliberate rather than comparing timestamps: the two
	// files are appended independently, asked_at/answered_at are optional and
	// written by whoever appends the line, and second-granularity RFC3339 from
	// two writers cannot order a fast exchange. Counting needs nothing but the
	// lines themselves. It fails toward OPEN - a re-ask asks again rather than
	// assuming the previous answer still applies - which is the safe direction
	// here and also the behaviour under a byte-truncated answers file.
	asks := make(map[string]int, len(questionLines))
	answersByID := make(map[string][]Answer, len(answerLines))
	for _, line := range questionLines {
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			conv.Notes = append(conv.Notes, "skipped a malformed questions.ndjson line")
			continue
		}
		q.ID = strings.TrimSpace(q.ID)
		if q.ID == "" {
			conv.Notes = append(conv.Notes, "skipped a questions.ndjson line with no id")
			continue
		}
		switch strings.TrimSpace(q.Kind) {
		case KindRetract:
			if entry, ok := byID[q.ID]; ok {
				entry.Retracted = true
				entry.Reason = q.Reason
			} else {
				conv.Notes = append(conv.Notes, fmt.Sprintf("retraction for unknown question %q ignored", q.ID))
			}
			continue
		case KindQuestion, "":
			q.Kind = KindQuestion
		default:
			conv.Notes = append(conv.Notes, fmt.Sprintf("skipped question %q with unknown kind %q", q.ID, q.Kind))
			continue
		}
		if strings.TrimSpace(q.Question) == "" {
			conv.Notes = append(conv.Notes, fmt.Sprintf("skipped question %q with no question text", q.ID))
			continue
		}
		// Routing by weight is the reviewer's own job, so a minor question is
		// never escalated on its behalf: emitting one is the protocol
		// violation, and reporting it keeps that visible instead of parking
		// the run on a question the reviewer was told to decide itself.
		if strings.EqualFold(strings.TrimSpace(q.Weight), WeightMinor) {
			conv.Notes = append(conv.Notes, fmt.Sprintf("dropped minor-weight question %q; the reviewer decides minor questions itself", q.ID))
			continue
		}
		asks[q.ID]++
		if entry, ok := byID[q.ID]; ok {
			// A later question line for the same id is an edit, not a
			// duplicate. It also revives a retracted question, because
			// re-asking is how the reviewer says the retraction was wrong.
			entry.Question = q
			entry.Retracted = false
			continue
		}
		entry := &Entry{Question: q}
		byID[q.ID] = entry
		order = append(order, q.ID)
	}

	for _, line := range answerLines {
		var a Answer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			conv.Notes = append(conv.Notes, "skipped a malformed answers.ndjson line")
			continue
		}
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" || strings.TrimSpace(a.Answer) == "" {
			conv.Notes = append(conv.Notes, "skipped an answers.ndjson line with no id or no answer")
			continue
		}
		if _, ok := byID[a.ID]; !ok {
			// The writer may be racing a question it has not read yet, or
			// answering something the reviewer withdrew. Recorded, ignored.
			conv.Notes = append(conv.Notes, fmt.Sprintf("answer for unknown question %q ignored", a.ID))
			continue
		}
		answersByID[a.ID] = append(answersByID[a.ID], a)
	}

	// Attach the newest answer only to a question that has been answered at
	// least as many times as it was asked; see the asks comment above. A
	// question left short of that stays OPEN and parks the gate again.
	for id, entry := range byID {
		answers := answersByID[id]
		if len(answers) == 0 || len(answers) < asks[id] {
			continue
		}
		answer := answers[len(answers)-1]
		entry.Answer = &answer
	}

	conv.Entries = make([]Entry, 0, len(order))
	for _, id := range order {
		conv.Entries = append(conv.Entries, *byID[id])
	}
	if qTruncated || aTruncated {
		conv.Notes = append(conv.Notes, "review conversation file exceeded its size bound; older lines were not read")
	}
	return conv, nil
}

// AppendQuestion appends one question or retraction, creating the directory on
// first use. It is the writer used by the pipeline's own tests and by any
// future tool use; the reviewer agent writes the same lines with its own file
// tools.
func AppendQuestion(dir string, q Question) error {
	if strings.TrimSpace(q.ID) == "" {
		return errors.New("review question requires an id")
	}
	if strings.TrimSpace(q.Kind) == "" {
		q.Kind = KindQuestion
	}
	if q.Kind == KindQuestion {
		if strings.TrimSpace(q.Question) == "" {
			return errors.New("review question requires question text")
		}
		if strings.TrimSpace(q.Weight) == "" {
			q.Weight = WeightMajor
		}
		if strings.TrimSpace(q.AskedAt) == "" {
			q.AskedAt = time.Now().UTC().Format(time.RFC3339)
		}
	} else if strings.TrimSpace(q.At) == "" {
		q.At = time.Now().UTC().Format(time.RFC3339)
	}
	return appendLine(dir, QuestionsFile, q)
}

// AppendAnswer appends one answer, creating the directory on first use.
func AppendAnswer(dir string, a Answer) error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("review answer requires a question id")
	}
	if strings.TrimSpace(a.Answer) == "" {
		return errors.New("review answer requires answer text")
	}
	if strings.TrimSpace(a.AnsweredAt) == "" {
		a.AnsweredAt = time.Now().UTC().Format(time.RFC3339)
	}
	return appendLine(dir, AnswersFile, a)
}

func appendLine(dir, name string, payload any) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("review conversation directory is not set")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode review conversation line: %w", err)
	}
	if len(encoded)+1 > maxLineBytes {
		return fmt.Errorf("review conversation line is %d bytes, over the %d byte limit", len(encoded), maxLineBytes)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create review conversation dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("append %s: %w", name, err)
	}
	return nil
}

// readLines returns the non-empty lines of an ndjson file, newest-last and
// bounded. A missing file is no lines and no error.
func readLines(path string) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	var lines []string
	truncated := false
	read := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		read += len(scanner.Bytes()) + 1
		if read > maxFileBytes {
			truncated = true
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		// A line over the scanner budget, or a torn read while the reviewer is
		// still appending. Keep what parsed rather than losing the file.
		truncated = true
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	return lines, truncated, nil
}
