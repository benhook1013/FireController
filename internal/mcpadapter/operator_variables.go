package mcpadapter

import (
	"context"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type varReadInput struct {
	Key *string `json:"key"`
}

func varReadArguments(data json.RawMessage) (arguments, error) {
	var in varReadInput
	if len(data) > 16<<10 || strictjson.Decode(data, &in) != nil || in.Key != nil && !client.ValidOperatorVariableKey(*in.Key) {
		return arguments{}, invalidArguments()
	}
	var args arguments
	if in.Key != nil {
		args.id = *in.Key
	}
	return args, nil
}

func runVarRead(ctx context.Context, service *client.Client, args arguments) (any, error) {
	if args.id == "" {
		return service.OperatorVariables(ctx)
	}
	return service.OperatorVariable(ctx, args.id)
}

func varReadDefinition() definition {
	return definition{"var_read", "Read operator variables: opaque short strings an administrator sets for agents (for example provider_mode). Omit key for every effective value; give key for one value plus its instance and project-override entries. FireController never interprets values. Read-only.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"key": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[a-z0-9][a-z0-9_.-]*$"}}}}
}
