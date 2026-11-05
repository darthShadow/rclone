package fs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type identityDir struct{ *Dir }

func (*identityDir) EntryIdentity() uint64 { return 42 }

func TestDirWrapperEntryIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		dir  Directory
		want uint64
	}{
		{"Forwards", &identityDir{NewDir("y", time.Now())}, 42},
		{"PlainDir", NewDir("y", time.Now()), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, NewDirWrapper("x", test.dir).EntryIdentity())
		})
	}
}
