package bencode

import (
	"bytes"
	"reflect"
	"testing"
)

func TestMarshal(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "spam", "4:spam"},
		{"empty string", "", "0:"},
		{"bytes", []byte("ab"), "2:ab"},
		{"int", 42, "i42e"},
		{"int64", int64(-7), "i-7e"},
		{"zero", int64(0), "i0e"},
		{"list", []any{"a", int64(1)}, "l1:ai1ee"},
		{"empty list", []any{}, "le"},
		{"dict", map[string]any{"b": int64(2), "a": "x"}, "d1:a1:x1:bi2ee"},
		{"empty dict", map[string]any{}, "de"},
		{"nested", map[string]any{"l": []any{map[string]any{"k": "v"}}}, "d1:lld1:k1:veee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("Marshal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarshalSortsDictKeys(t *testing.T) {
	got, err := Marshal(map[string]any{"zebra": int64(1), "alpha": int64(2), "mid": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "d5:alphai2e3:midi3e5:zebrai1ee" {
		t.Fatalf("keys are not in sorted order: %q", got)
	}
}

func TestMarshalRejectsUnsupportedType(t *testing.T) {
	if _, err := Marshal(3.5); err == nil {
		t.Fatal("expected an error for a float")
	}
	if _, err := Marshal(map[string]any{"k": struct{}{}}); err == nil {
		t.Fatal("expected an error for a struct inside a dict")
	}
}

func TestUnmarshal(t *testing.T) {
	t.Run("integer", func(t *testing.T) {
		v, err := Unmarshal([]byte("i-15e"))
		if err != nil {
			t.Fatal(err)
		}
		if v != int64(-15) {
			t.Fatalf("got %v, want -15", v)
		}
	})

	t.Run("string", func(t *testing.T) {
		v, err := Unmarshal([]byte("4:spam"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(v.([]byte), []byte("spam")) {
			t.Fatalf("got %q, want spam", v)
		}
	})

	t.Run("list", func(t *testing.T) {
		v, err := Unmarshal([]byte("l1:ai1ee"))
		if err != nil {
			t.Fatal(err)
		}
		want := []any{[]byte("a"), int64(1)}
		if !reflect.DeepEqual(v, want) {
			t.Fatalf("got %#v, want %#v", v, want)
		}
	})

	t.Run("dict", func(t *testing.T) {
		v, err := Unmarshal([]byte("d1:a1:x1:bi2ee"))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"a": []byte("x"), "b": int64(2)}
		if !reflect.DeepEqual(v, want) {
			t.Fatalf("got %#v, want %#v", v, want)
		}
	})
}

func TestUnmarshalRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"invalid token", "x"},
		{"unterminated integer", "i42"},
		{"non numeric integer", "iabce"},
		{"string longer than input", "10:short"},
		{"string without colon", "12"},
		{"unterminated list", "l1:a"},
		{"unterminated dict", "d1:a1:b"},
		{"dict key not a string", "di1ei2ee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Unmarshal([]byte(tc.in)); err == nil {
				t.Fatalf("expected an error for %q", tc.in)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	original := map[string]any{
		"announce": "http://192.0.2.1:6969/announce",
		"info": map[string]any{
			"name":         "qbench",
			"piece length": int64(262144),
			"length":       int64(1073741824),
			"pieces":       []byte{0x00, 0xff, 0x10},
		},
	}
	raw, err := Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	top := decoded.(map[string]any)
	if string(top["announce"].([]byte)) != original["announce"] {
		t.Fatalf("announce did not survive: %q", top["announce"])
	}
	info := top["info"].(map[string]any)
	if info["piece length"].(int64) != 262144 || info["length"].(int64) != 1073741824 {
		t.Fatalf("integers did not survive: %#v", info)
	}
	if !bytes.Equal(info["pieces"].([]byte), []byte{0x00, 0xff, 0x10}) {
		t.Fatalf("binary data did not survive: %#v", info["pieces"])
	}

	reencoded, err := Marshal(map[string]any{
		"announce": top["announce"],
		"info": map[string]any{
			"name":         info["name"],
			"piece length": info["piece length"],
			"length":       info["length"],
			"pieces":       info["pieces"],
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, reencoded) {
		t.Fatal("re-encoding a decoded value produced different bytes")
	}
}
