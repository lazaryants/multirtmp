package main

import "testing"

func TestUIURL(t *testing.T) {
	cases := []struct {
		addr, token, want string
	}{
		{"127.0.0.1:8080", "", "http://127.0.0.1:8080/"},
		{"localhost:8080", "", "http://127.0.0.1:8080/"},
		{"0.0.0.0:8080", "tok", "http://127.0.0.1:8080/"},
		{":8080", "tok", "http://127.0.0.1:8080/"},
		{"[::]:8080", "tok", "http://127.0.0.1:8080/"},
		{"[::1]:8080", "", "http://[::1]:8080/"},
		{"127.0.0.5:9000", "", "http://127.0.0.5:9000/"},
		{"192.168.1.10:8080", "a+b/c", "http://192.168.1.10:8080/?token=a%2Bb%2Fc"},
		{"studio-pc:8080", "tok", "http://studio-pc:8080/?token=tok"},
	}

	for _, tc := range cases {
		got, err := uiURL(tc.addr, tc.token)
		if err != nil {
			t.Fatalf("uiURL(%q): %v", tc.addr, err)
		}
		if got != tc.want {
			t.Errorf("uiURL(%q, %q) = %q, want %q", tc.addr, tc.token, got, tc.want)
		}
	}

	if _, err := uiURL("no-port", ""); err == nil {
		t.Error("uiURL without port: expected error")
	}
}
