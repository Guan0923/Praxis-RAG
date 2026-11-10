package config

import "testing"

func TestParsePDFEngine(t *testing.T) {
	cases := []struct {
		raw          string
		parserPython string
		want         string
		wantErr      bool
	}{
		{raw: "", parserPython: "", want: PDFEngineGo},
		{raw: "", parserPython: "C:/parser/python.exe", want: PDFEngineDocling},
		{raw: "go", parserPython: "C:/parser/python.exe", want: PDFEngineGo},
		{raw: "DOCLING", parserPython: "", want: PDFEngineDocling},
		{raw: "docling", parserPython: "", want: PDFEngineDocling},
		{raw: "other", wantErr: true},
	}
	for _, testCase := range cases {
		got, err := parsePDFEngine(testCase.raw, testCase.parserPython)
		if testCase.wantErr {
			if err == nil {
				t.Errorf("parsePDFEngine(%q, %q) error = nil, want error", testCase.raw, testCase.parserPython)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePDFEngine(%q, %q) error = %v", testCase.raw, testCase.parserPython, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("parsePDFEngine(%q, %q) = %q, want %q", testCase.raw, testCase.parserPython, got, testCase.want)
		}
	}
}
