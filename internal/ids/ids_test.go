package ids

import (
	"strings"
	"testing"
)

func TestNewAndUUIDPart(t *testing.T) {
	id := New("m")
	if !strings.HasPrefix(id, "m_") {
		t.Fatalf("id=%s", id)
	}
	u, err := UUIDPart(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(u) != 36 {
		t.Fatalf("uuid=%s", u)
	}
}

func TestInvalidIDs(t *testing.T) {
	for _, s := range []string{"", "m_", "m_not-a-uuid", "noseparator"} {
		if err := Validate(s); err == nil {
			t.Fatalf("expected %q to be invalid", s)
		}
	}
}

func TestUUIDPartRejectsEmptyPrefix(t *testing.T) {
	if _, err := UUIDPart("_" + NewUUID()); err == nil {
		t.Fatal("empty typed prefix was accepted")
	}
}
