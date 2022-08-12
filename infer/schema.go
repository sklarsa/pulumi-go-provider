// Copyright 2022, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infer

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/pulumi/pulumi-go-provider/internal/introspect"
	sch "github.com/pulumi/pulumi-go-provider/middleware/schema"
)

func getAnnotated(t reflect.Type) introspect.Annotator {
	// If we have type *R with value(i) = nil, NewAnnotator will fail. We need to get
	// value(i) = *R{}, so we reinflate the underlying value
	for t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Pointer {
		t = t.Elem()
	}
	i := reflect.New(t).Elem()
	if i.Kind() == reflect.Pointer && i.IsNil() {
		i = reflect.New(i.Type().Elem())
	}

	if i.Kind() != reflect.Pointer {
		v := reflect.New(i.Type())
		v.Elem().Set(i)
		i = v
	}

	if r, ok := i.Interface().(Annotated); ok {
		a := introspect.NewAnnotator(r)
		r.Annotate(&a)
		return a
	}

	// We want public fields to be filled in so we can index them without a nil check.
	return introspect.Annotator{
		Descriptions: map[string]string{},
		Defaults:     map[string]any{},
		DefaultEnvs:  map[string][]string{},
	}
}

func getResourceSchema[R, I, O any](isComponent bool) (schema.ResourceSpec, multierror.Error) {
	var r R
	var errs multierror.Error
	descriptions := getAnnotated(reflect.TypeOf(r))

	properties, required, err := propertyListFromType(reflect.TypeOf(new(O)), isComponent)
	if err != nil {
		var o O
		errs.Errors = append(errs.Errors, fmt.Errorf("could not serialize output type %T: %w", o, err))
	}

	inputProperties, requiredInputs, err := propertyListFromType(reflect.TypeOf(new(I)), isComponent)
	if err != nil {
		var i I
		errs.Errors = append(errs.Errors, fmt.Errorf("could not serialize input type %T: %w", i, err))
	}

	return schema.ResourceSpec{
		ObjectTypeSpec: schema.ObjectTypeSpec{
			Properties:  properties,
			Description: descriptions.Descriptions[""],
			Required:    required,
		},
		InputProperties: inputProperties,
		RequiredInputs:  requiredInputs,
		IsComponent:     isComponent,
	}, errs
}

func serializeTypeAsPropertyType(t reflect.Type, indicatePlain bool, extType string) (schema.TypeSpec, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if enum, ok := isEnum(t); ok {
		return schema.TypeSpec{
			Ref: "#/types/" + enum.token,
		}, nil
	}
	if tk, ok, err := resourceReferenceToken(t, extType, false); ok {
		if err != nil {
			return schema.TypeSpec{}, err
		}
		return tk, nil
	}
	if tk, ok, err := structReferenceToken(t); ok {
		if err != nil {
			return schema.TypeSpec{}, err
		}
		return schema.TypeSpec{
			Ref: "#/types/" + tk.String(),
		}, nil
	}

	// Must be a primitive type
	t, inputy, err := underlyingType(t)
	if err != nil {
		return schema.TypeSpec{}, err
	}

	primitive := func(t string) (schema.TypeSpec, error) {
		return schema.TypeSpec{Type: t, Plain: !inputy && indicatePlain}, nil
	}

	switch t.Kind() {
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return schema.TypeSpec{}, fmt.Errorf("map keys must be strings, found %s", t.Key().String())
		}
		el, err := serializeTypeAsPropertyType(t.Elem(), indicatePlain, extType)
		if err != nil {
			return schema.TypeSpec{}, err
		}
		return schema.TypeSpec{
			Type:                 "object",
			AdditionalProperties: &el,
		}, nil
	case reflect.Array, reflect.Slice:
		el, err := serializeTypeAsPropertyType(t.Elem(), indicatePlain, extType)
		if err != nil {
			return schema.TypeSpec{}, err
		}
		return schema.TypeSpec{
			Type:  "array",
			Items: &el,
		}, nil
	case reflect.Bool:
		return primitive("boolean")
	case reflect.Int, reflect.Int64, reflect.Int32:
		return primitive("integer")
	case reflect.Float64:
		return primitive("number")
	case reflect.String:
		return primitive("string")
	case reflect.Interface:
		return schema.TypeSpec{
			Ref: "pulumi.json#/Any",
		}, nil
	default:
		return schema.TypeSpec{}, fmt.Errorf("unknown type: '%s'", t.String())
	}
}

// underlyingType find the non-inputty, non-ptr type of t. It returns the underlying type
// and if t was an Inputty or Outputty type.
func underlyingType(t reflect.Type) (reflect.Type, bool, error) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	isInputType := t.Implements(reflect.TypeOf(new(pulumi.Input)).Elem())
	_, isOutputType := reflect.New(t).Interface().(pulumi.Output)

	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if isOutputType {
		t = reflect.New(t).Elem().Interface().(pulumi.Output).ElementType()
	} else if isInputType {
		T := t.Name()
		if strings.HasSuffix(T, "Input") {
			T = strings.TrimSuffix(T, "Input")
		} else {
			return nil, false, fmt.Errorf("%v is an input type, but does not end in \"Input\"", T)
		}
		toOutMethod, ok := t.MethodByName("To" + T + "Output")
		if !ok {
			return nil, false, fmt.Errorf("%v is an input type, but does not have a To%vOutput method", t.Name(), T)
		}
		outputT := toOutMethod.Type.Out(0)
		//create new object of type outputT
		strct := reflect.New(outputT).Elem().Interface()
		out, ok := strct.(pulumi.Output)
		if !ok {
			return nil, false, fmt.Errorf("return type %s of method To%vOutput on type %v does not implement Output",
				reflect.TypeOf(strct), T, t.Name())
		}
		t = out.ElementType()
	}

	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t, isOutputType || isInputType, nil
}

func propertyListFromType(typ reflect.Type, indicatePlain bool) (
	props map[string]schema.PropertySpec, required []string, err error) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	props = map[string]schema.PropertySpec{}
	annotations := getAnnotated(typ)

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		tags, err := introspect.ParseTag(field)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid fields '%s' on '%s': %w", field.Name, typ, err)
		}
		if tags.Internal {
			continue
		}
		serialized, err := serializeTypeAsPropertyType(fieldType, indicatePlain, tags.ExternalType)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid type '%s' on '%s.%s': %w", fieldType, typ, field.Name, err)
		}
		if !tags.Optional {
			required = append(required, tags.Name)
		}
		spec := &schema.PropertySpec{
			TypeSpec:         serialized,
			Secret:           tags.Secret,
			ReplaceOnChanges: tags.ReplaceOnChanges,
			Description:      annotations.Descriptions[tags.Name],
			Default:          annotations.Defaults[tags.Name],
		}
		if envs := annotations.DefaultEnvs[tags.Name]; len(envs) > 0 {
			spec.DefaultInfo = &schema.DefaultSpec{
				Environment: envs,
			}
		}
		props[tags.Name] = *spec
	}
	return props, required, nil
}

func resourceReferenceToken(t reflect.Type, extTag string, allowMissingExtType bool) (schema.TypeSpec, bool, error) {
	ptrT := reflect.PointerTo(t)
	implements := func(typ reflect.Type) bool {
		return t.Implements(typ) || ptrT.Implements(typ)
	}
	switch {
	// This handles both components and resources
	case implements(reflect.TypeOf(new(sch.Resource)).Elem()):
		tk, err := reflect.New(t).Elem().Interface().(sch.Resource).GetToken()
		return schema.TypeSpec{
			Ref: "#/resources/" + tk.String(),
		}, true, err
	case implements(reflect.TypeOf(new(pulumi.Resource)).Elem()):
		// This is an external resource
		if extTag == "" {
			if allowMissingExtType {
				return schema.TypeSpec{}, true, nil
			}
			return schema.TypeSpec{}, true, fmt.Errorf("missing type= tag on foreign resource %s", t)
		}
		parts := strings.Split(extTag, ":") // pkg@version:module:name
		contract.Assertf(len(parts) == 3, "invalid type= tag; got %q", extTag)
		head := strings.Split(parts[0], "@") // pkg@version
		contract.Assertf(len(head) == 2, "invalid type= head; got %q", parts[0])
		pkgName := head[0]
		pkgVersion := head[1]
		module := parts[1]
		name := parts[2]
		tk := fmt.Sprintf("%s:%s:%s", pkgName, module, name)
		return schema.TypeSpec{
			Ref: fmt.Sprintf("/%s/%s/schema.json#/resources/%s", pkgName, pkgVersion, tk),
		}, true, nil
	default:
		return schema.TypeSpec{}, false, nil
	}
}

func structReferenceToken(t reflect.Type) (tokens.Type, bool, error) {
	if t.Kind() != reflect.Struct ||
		t.Implements(reflect.TypeOf(new(pulumi.Output)).Elem()) {
		return "", false, nil
	}
	tk, err := introspect.GetToken("pkg", reflect.New(t).Elem().Interface())
	return tk, true, err
}

func schemaNameForType(t reflect.Kind) string {
	switch t {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Float64:
		return "number"
	case reflect.Int:
		return "integer"
	default:
		panic(fmt.Sprintf("unknown primitive type: %s", t))
	}
}