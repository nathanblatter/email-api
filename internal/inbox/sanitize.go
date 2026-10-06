package inbox

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Sanitize is the defence-in-depth stage between a received message and any
// model that reads it. It cannot make untrusted text safe; it narrows the
// channel (invisible characters, hidden HTML) and labels what looks like an
// attempt to instruct the reader, so agents and the UI can treat it as data.
type Sanitized struct {
	Text       string   // cleaned plain text with hidden HTML content removed
	HiddenText []string // text that was present in HTML but invisible to a human
	Injection  bool     // instruction-shaped content found
	Reasons    []string // what triggered the flag (short, human-readable)
}

// CleanText normalises to NFKC (folds full-width and other look-alike
// forms) and drops characters that render as nothing: zero-width and
// joiner characters, bidi controls, soft hyphens, Unicode tag characters,
// and other format/control code points except ordinary whitespace.
func CleanText(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteRune(r)
		case r == 0xAD, r == 0xFEFF, r == 0x200B, r == 0x200C, r == 0x200D, r == 0x2060, r == 0x180E:
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // bidi overrides/isolates
		case r >= 0xE0000 && r <= 0xE007F: // tag characters
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Cc, r), unicode.Is(unicode.Co, r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.ToValidUTF8(b.String(), "")
}

// hiddenStyle matches inline styles that make an element invisible.
var hiddenStyle = regexp.MustCompile(`(?i)(display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0(px|pt|em|%)?\b|opacity\s*:\s*0(\.0+)?\s*(;|$|")|(max-)?height\s*:\s*0(px)?\b|(max-)?width\s*:\s*0(px)?\b|left\s*:\s*-\d{3,}|top\s*:\s*-\d{3,}|text-indent\s*:\s*-\d{3,}|color\s*:\s*(#fff(fff)?|white|transparent|rgba?\(\s*255\s*,\s*255\s*,\s*255)\b)`)

// hiddenElem captures elements that carry a hidden style, a hidden attribute
// or aria-hidden, with their (non-nested) inner text. Nested cases fall
// through to the inner match, which is enough: the goal is to surface text
// a human cannot see, not to parse HTML.
var (
	hiddenElem   = regexp.MustCompile(`(?is)<(\w+)\b([^>]*?)>(.*?)</\s*\w+\s*>`)
	htmlComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	anyTag       = regexp.MustCompile(`<[^>]+>`)
	hiddenAttr   = regexp.MustCompile(`(?i)\s(hidden|aria-hidden\s*=\s*["']?true)`)
	styleAttr    = regexp.MustCompile(`(?i)\sstyle\s*=\s*["']([^"']*)["']`)
	multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)
	condComment  = regexp.MustCompile(`^(\[if [^\]]*\]>?\s*)?(<!\[endif\]|\[endif\])?\s*(<!)?$|^\[if [^\]]*\]>\s*<!$`)
)

// StripHiddenHTML removes comments and elements a human would not see and
// returns the HTML without them plus the text they contained.
func StripHiddenHTML(h string) (clean string, hidden []string) {
	add := func(s string) {
		s = strings.TrimSpace(multiSpaceRe.ReplaceAllString(anyTag.ReplaceAllString(s, " "), " "))
		// Outlook conditional-comment markers ("[if mso]>", "<![endif]") are
		// plumbing, not hidden prose.
		if s == "" || condComment.MatchString(s) {
			return
		}
		hidden = append(hidden, s)
	}
	clean = htmlComment.ReplaceAllStringFunc(h, func(c string) string {
		add(strings.TrimSuffix(strings.TrimPrefix(c, "<!--"), "-->"))
		return ""
	})
	for i := 0; i < 5; i++ { // a few passes catch hidden elements inside hidden elements
		changed := false
		clean = hiddenElem.ReplaceAllStringFunc(clean, func(m string) string {
			sub := hiddenElem.FindStringSubmatch(m)
			attrs, inner := sub[2], sub[3]
			isHidden := hiddenAttr.MatchString(attrs)
			if st := styleAttr.FindStringSubmatch(attrs); st != nil && hiddenStyle.MatchString(st[1]) {
				isHidden = true
			}
			if !isHidden {
				return m
			}
			changed = true
			add(inner)
			return ""
		})
		if !changed {
			break
		}
	}
	return clean, hidden
}

// injectionPatterns are instruction-shaped phrasings aimed at a model reader
// rather than a human. Deliberately conservative: each is something a normal
// correspondent would almost never write.
var injectionPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.\n]{0,40}\b(previous|prior|above|earlier|all|your|the)\b[^.\n]{0,20}\b(instructions?|prompts?|rules|guidelines|directions)\b`), "asks the reader to ignore its instructions"},
	{regexp.MustCompile(`(?i)\b(you are|you're|act as|pretend to be|roleplay as)\b[^.\n]{0,30}\b(an? )?(ai|assistant|llm|language model|chatbot|claude|chatgpt|gpt)\b`), "addresses the reader as an AI"},
	{regexp.MustCompile(`(?i)\b(system prompt|developer mode|jailbreak|do anything now|\bDAN\b)`), "references system prompts or jailbreaks"},
	{regexp.MustCompile(`(?i)\b(call|invoke|run|execute|use)\s+(the\s+|a\s+|your\s+)?([\w-]+_[\w-]+\s+|[\w-]+\s+)?(tool|function|mcp)s?\b`), "tells the reader to call a tool"},
	{regexp.MustCompile(`(?i)\b(do not|don't|never)\b[^.\n]{0,20}\b(tell|inform|show|alert|notify)\b[^.\n]{0,15}\b(the )?(user|human|owner|nathan)\b`), "asks the reader to hide something from the user"},
	{regexp.MustCompile(`(?i)\b(forward|send|email|exfiltrate|transmit|post)\b[^.\n]{0,40}\b(api[ -]?keys?|passwords?|secrets?|tokens?|credentials?|all (your )?(emails?|messages?|contacts?))\b`), "asks for secrets or data to be sent"},
	{regexp.MustCompile(`(?i)(<\|im_start\|>|<\|im_end\|>|\[INST\]|\[/INST\]|<<SYS>>|<\|system\|>|<\|assistant\|>|###\s*(system|instruction|assistant)\b|^\s*(system|assistant)\s*:)`), "contains model prompt markup"},
	{regexp.MustCompile(`(?i)\b(this is (an? )?(important |urgent )?(instruction|command|directive) (for|to) (the|any|all) (ai|assistant|agent|model|bot)s?)\b`), "labels itself as an instruction for an AI"},
	{regexp.MustCompile(`(?i)\b(when (you|an? (ai|assistant|agent)) (reads?|process(es)?|summari[sz]es?) this (email|message))\b`), "scripts behaviour for whoever reads it"},
}

// detectInjection scans text and returns the distinct reasons that matched.
func detectInjection(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range injectionPatterns {
		if p.re.MatchString(text) && !seen[p.reason] {
			seen[p.reason] = true
			out = append(out, p.reason)
		}
	}
	return out
}

// Sanitize produces the agent-facing view of a parsed message: cleaned text
// with hidden HTML content removed, the hidden snippets for the record, and
// an injection verdict computed over everything a model might read, hidden
// parts and subject included.
func Sanitize(p *Parsed) Sanitized {
	var s Sanitized
	cleanHTML, hidden := StripHiddenHTML(p.HTML)
	for _, h := range hidden {
		s.HiddenText = append(s.HiddenText, CleanText(h))
	}
	text := p.Text
	if (p.TextFromHTML || strings.TrimSpace(text) == "") && cleanHTML != "" {
		text = htmlToText(cleanHTML)
	}
	s.Text = strings.TrimSpace(CleanText(text))
	// The plain-text part may carry content the HTML part hides, or vice
	// versa; scan all of it plus the subject.
	scan := strings.Join(append([]string{p.Subject, s.Text, CleanText(htmlToText(p.HTML))}, s.HiddenText...), "\n")
	s.Reasons = detectInjection(scan)
	if len(s.HiddenText) > 0 && len(strings.Join(s.HiddenText, "")) > 40 {
		s.Reasons = append(s.Reasons, "contains text hidden from human readers")
	}
	s.Injection = len(s.Reasons) > 0
	return s
}
