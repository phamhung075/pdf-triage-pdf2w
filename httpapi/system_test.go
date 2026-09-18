package httpapi

import "testing"

// TestFormatBytesJSToFixed pins the bonus parity item: formatBytes reimplements JS
// parseFloat(x.toFixed(2)), and strconv.FormatFloat rounds exact ties to even while ECMAScript
// toFixed rounds ties to the larger value. Every expected string below was produced by running the
// real route algorithm from web-server.ts:376-382 through `node -e`:
//
//	const formatBytes = (bytes) => {
//	  if (bytes === 0) return '0 B';
//	  const k = 1024, sizes = ['B','KB','MB','GB','TB'];
//	  const i = Math.floor(Math.log(bytes) / Math.log(k));
//	  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
//	};
//
// The 1.125/1.625/2.125/1023.125 KB and 1.125 MB/1.125 GB rows are the exact-half decimals where
// the old Go code answered 1.12/1.62/2.12/1023.12/1.12; 1.005 KB (bytes 1029) and 1023.995 KB
// (bytes 1048571) are the near-boundary values named in the task, scaled to the byte-size
// formatting the route uses. The four task-named decimals are also checked directly against
// jsToFixed2 below, including 0.125, which formatBytes never displays because its bracket always
// yields a value in [1, 1024).
func TestFormatBytesJSToFixed(t *testing.T) {
	direct := []struct {
		value float64
		want  string
	}{
		{value: 1.005, want: "1.00"},
		{value: 2.5, want: "2.50"},
		{value: 0.125, want: "0.13"},
		{value: 1023.995, want: "1024.00"},
	}
	for _, tc := range direct {
		if got := jsToFixed2(tc.value); got != tc.want {
			t.Errorf("jsToFixed2(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}

	cases := []struct {
		bytes int64
		want  string
	}{
		{bytes: 0, want: "0 B"},
		{bytes: 1, want: "1 B"},
		{bytes: 10, want: "10 B"},
		{bytes: 15, want: "15 B"},
		{bytes: 128, want: "128 B"},
		{bytes: 1023, want: "1023 B"},
		{bytes: 1024, want: "1 KB"},
		{bytes: 1025, want: "1 KB"},
		{bytes: 1029, want: "1 KB"},
		{bytes: 1152, want: "1.13 KB"},
		{bytes: 1408, want: "1.38 KB"},
		{bytes: 1664, want: "1.63 KB"},
		{bytes: 1920, want: "1.88 KB"},
		{bytes: 2176, want: "2.13 KB"},
		{bytes: 2560, want: "2.5 KB"},
		{bytes: 3584, want: "3.5 KB"},
		{bytes: 131072, want: "128 KB"},
		{bytes: 1047680, want: "1023.13 KB"},
		{bytes: 1048448, want: "1023.88 KB"},
		{bytes: 1048570, want: "1023.99 KB"},
		{bytes: 1048571, want: "1024 KB"},
		{bytes: 1073610752, want: "1023.88 MB"},
		{bytes: 1073731891, want: "1023.99 MB"},
		{bytes: 1179648, want: "1.13 MB"},
		{bytes: 1441792, want: "1.38 MB"},
		{bytes: 1207959552, want: "1.13 GB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.bytes); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
