package client

import "fmt"

// Field paths are supplied by these typed validators, never by request text.
// Do not echo the malformed commit or the potentially private proof body.
func reviewFixSHAValidation(prefix, expected, fix string) error {
	for _, field := range []struct {
		name, value string
		empty       bool
	}{{"expected_sha", expected, true}, {"fix_sha", fix, false}} {
		if field.empty && field.value == "" || controlHex(field.value, 40) {
			continue
		}
		name := prefix + field.name
		format := "exactly 40 lowercase hexadecimal characters"
		if field.empty {
			format = "empty or " + format
		}
		err := fail("validation_failed", fmt.Sprintf("%s must be %s.", name, format), "Use the full commit SHA; abbreviated SHAs are not accepted. No request was sent.", 400)
		err.Details = map[string]string{"field": name, "format": format}
		return err
	}
	return nil
}
