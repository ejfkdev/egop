package remote

import (
	"testing"

	"github.com/ejfkdev/egop/contract"
)

func TestDecodeEnvelopePreservesErrorDetail(t *testing.T) {
	expected := contract.ErrorDetail{
		Code: "schema.input.invalid", Phase: "input", Plugin: "p", Fn: "f",
		Path: "input", Retryable: false, Message: "invalid input",
	}
	payload := errEnvelope(contract.NewCallError(expected))
	if _, err := decodeEnvelope(payload); err == nil {
		t.Fatal("error envelope must fail")
	} else {
		actual, ok := contract.ErrorDetailOf(err)
		if !ok || actual.Code != expected.Code || actual.Plugin != expected.Plugin || actual.Fn != expected.Fn || actual.Path != expected.Path || actual.Retryable {
			t.Fatalf("error detail = %+v, ok=%v", actual, ok)
		}
	}
}
