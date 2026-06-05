package load

import "testing"

func TestMessageRoundTrip(t *testing.T) {
	in := Message{IntendedMonoNs: 123456789, Seq: 42, PartitionIndex: 7}
	buf := in.Encode()
	if len(buf) != PayloadSize {
		t.Fatalf("payload size = %d, want %d", len(buf), PayloadSize)
	}
	out, err := Decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestDecodeRejectsShort(t *testing.T) {
	if _, err := Decode([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on short buffer")
	}
}
