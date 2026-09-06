package sourcemap

import "fmt"

const b64chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64index = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(b64chars); i++ {
		t[b64chars[i]] = int8(i)
	}
	return t
}()

// decodeVLQSegment decodes one comma-free VLQ segment (the fields of one
// mapping) into signed integers.
func decodeVLQSegment(s string) ([]int64, error) {
	var out []int64
	var value int64
	var shift uint
	for i := 0; i < len(s); i++ {
		d := b64index[s[i]]
		if d < 0 {
			return nil, fmt.Errorf("invalid base64 char %q", s[i])
		}
		value |= int64(d&31) << shift
		if d&32 != 0 {
			shift += 5
			if shift > 60 {
				return nil, fmt.Errorf("VLQ value too large")
			}
			continue
		}
		// Sign bit is the LSB of the accumulated value.
		v := value >> 1
		if value&1 != 0 {
			v = -v
		}
		out = append(out, v)
		value, shift = 0, 0
	}
	if shift != 0 {
		return nil, fmt.Errorf("truncated VLQ segment")
	}
	return out, nil
}
