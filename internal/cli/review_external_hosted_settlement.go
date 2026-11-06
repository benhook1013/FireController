package cli

import (
	"strconv"

	"github.com/benhook1013/FireController/internal/client"
)

func parseExternalHostedSettlement(args []string) (*mutationInvocation, bool) {
	values, ok := reviewSourceFlags(args, "--pr", "--file", "--key")
	if !ok || !validMutationKey(values["--key"]) || values["--file"] == "" {
		return nil, false
	}
	number, err := strconv.ParseInt(values["--pr"], 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != values["--pr"] {
		return nil, false
	}
	call := &mutationInvocation{operation: "reviews settle-hosted", key: values["--key"], pullNumber: number}
	data, ok, diagnostic := readPrivateTextFileArgument(values["--file"], client.ExternalHostedSettlementMaxBytes, "--file")
	if !ok {
		call.argumentDiagnostic = diagnostic
		return call, false
	}
	call.externalHostedSettlement, err = client.DecodeExternalHostedSettlementRequest([]byte(data))
	call.argumentDiagnostic = reviewFieldArgumentDiagnostic(err)
	return call, err == nil
}
