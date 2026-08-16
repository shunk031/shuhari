package schemas

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed *.schema.json
var files embed.FS

func Validate(name string, value any) error {
	path := name + ".schema.json"
	contents, err := files.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s contract: %w", name, err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("decode %s contract: %w", name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(path, document); err != nil {
		return fmt.Errorf("load %s contract: %w", name, err)
	}
	contract, err := compiler.Compile(path)
	if err != nil {
		return fmt.Errorf("compile %s contract: %w", name, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s artifact: %w", name, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode %s artifact: %w", name, err)
	}
	if err := contract.Validate(instance); err != nil {
		return fmt.Errorf("validate %s artifact: %w", name, err)
	}
	return nil
}
