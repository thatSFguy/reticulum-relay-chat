package hub

import (
	"strings"
	"unicode"
)

// Names that render the same are not different names.
//
// Uniqueness used to compare the lowercased STRING, which is a fact
// about bytes, and the thing a person picks a name out of a list by is
// a fact about pixels. Those come apart in two ways that need no
// sophistication to exploit:
//
//	"sam​"   a zero-width space — same glyphs, different string
//	"sаm"         a Cyrillic а — same glyphs, different string
//
// Either one gets you a second "sam" in /who, in a room, and in the
// peer directory, and nobody reading the list can tell which is which.
// Worse than a nuisance: the whole point of unique nicks is that a
// mention resolves to ONE person, and a reader picking the wrong "sam"
// to reply to has no way of noticing.
//
// So the hub compares names by what they LOOK like. nickKey folds a
// name to a comparison key, assignNickLocked suffixes anything that
// collides on it, and the result is that two names that render alike
// cannot both exist — the second is sam1, which is visibly different,
// which is the only property that helps a person reading the room.
//
// The display name keeps the characters its owner typed. This is
// deliberate: folding for COMPARISON costs a Cyrillic speaker nothing,
// while folding for DISPLAY would rewrite their name into somebody
// else's alphabet.

// stripInvisible removes runes that occupy no space on screen: the
// Cf format characters (zero-width space and joiner, word joiner, BOM,
// soft hyphen, and the bidi overrides that can reverse how a name
// reads) and the Cc controls.
//
// They are removed from the GRANTED name, not merely from the key. A
// name whose difference from another name is invisible is not a
// different name, and keeping the character would leave the hub
// displaying two identical rows and calling them distinct.
//
// One casualty worth naming: U+200D joins emoji into a single glyph, so
// a family emoji in a nick becomes its component people. That is a fair
// price for the bidi override going away.
func stripInvisible(s string) string {
	if !strings.ContainsFunc(s, isInvisible) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isInvisible(r) {
			return -1
		}
		return r
	}, s)
}

// Whitespace is excluded even though a tab is Cc: it has its own rule
// (underscoreSpaces, which makes a name mentionable) and that rule runs
// after this one. Deleting the tab here would silently turn
// "sam\tjones" into "samjones" instead of "sam_jones" — a
// test caught exactly that.
func isInvisible(r rune) bool {
	if unicode.IsSpace(r) {
		return false
	}
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r)
}

// nickKey folds a name to what it looks like: invisibles gone,
// fullwidth forms narrowed, the common cross-script homoglyphs mapped
// to the Latin letter they imitate, and the whole thing lowercased.
//
// Two names with the same key cannot both be granted.
//
// INCOMPLETE, and honestly so. The full answer is UTS #39's confusables
// table — thousands of entries, a data file to vendor and keep current.
// This covers what is actually used to imitate a Latin name: the
// fullwidth block, and the Cyrillic and Greek letters that ARE Latin
// letters on screen. A determined imitation in a script not listed here
// still gets through, and the backstop for that is the same as for
// every other trust question on a hub — the identity hash, which
// /seen and a @hashprefix mention both speak.
func nickKey(s string) string {
	s = stripInvisible(s)
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E: // fullwidth ASCII
			b.WriteRune(r - 0xFEE0)
		default:
			if folded, ok := confusables[r]; ok {
				b.WriteRune(folded)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// confusables maps lowercase non-Latin letters to the Latin letter they
// are indistinguishable from in an ordinary UI font. Lowercase only —
// nickKey lowercases first, and Cyrillic А lowercases to а.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'б': '6', 'в': 'b', 'е': 'e', 'ѕ': 's', 'з': '3',
	'и': 'u', 'і': 'i', 'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h',
	'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x',
	'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w', 'ѡ': 'w', 'ғ': 'f',
	// Greek
	'α': 'a', 'β': 'b', 'ε': 'e', 'ζ': 'z', 'η': 'n', 'ι': 'i',
	'κ': 'k', 'μ': 'u', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't',
	'υ': 'u', 'χ': 'x', 'γ': 'y', 'σ': 'o', 'ϲ': 'c', 'ϳ': 'j',
	// Latin letters that imitate other Latin letters
	'ı': 'i', 'ɑ': 'a', 'ɡ': 'g', 'ɩ': 'i', 'ᴏ': 'o', 'ⅼ': 'l',
	'ѐ': 'e', 'ⲟ': 'o', 'ꓐ': 'b', 'ꞑ': 'n',
}
