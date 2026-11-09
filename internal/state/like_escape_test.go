package state

import "testing"

// TestEscapeLikeUsesPortableEscapeCharacter locks the LIKE escaping used by the
// document-task correlation queries to a character both SQLite and MySQL accept.
// The previous backslash literal (ESCAPE '\') is valid SQLite but a MySQL syntax
// error, which only the live MySQL run exposed.
func TestEscapeLikeUsesPortableEscapeCharacter(t *testing.T) {
	if got, want := escapeLike(`50%_off!`), `50!%!_off!!`; got != want {
		t.Fatalf("escapeLike() = %q, want %q", got, want)
	}
	if got, want := escapeLike(`document-task:plain:`), `document-task:plain:`; got != want {
		t.Fatalf("escapeLike() = %q, want %q", got, want)
	}
}
