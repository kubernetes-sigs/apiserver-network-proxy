package e2e

import (
	"bytes"
	"fmt"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KeyValue struct {
	Key   string
	Value string
}

type DeploymentConfig struct {
	Replicas int
	Image    string
	Args     []KeyValue
}

var (
	serverTemplates = template.Must(template.ParseGlob("templates/server/*"))
	agentTemplates  = template.Must(template.ParseGlob("templates/agent/*"))
)

// renderServerTemplate renders a template from e2e/templates/server into a kubernetes object.
func renderServerTemplate(file string, params any) (client.Object, *schema.GroupVersionKind, error) {
	b := &bytes.Buffer{}

	err := serverTemplates.ExecuteTemplate(b, file, params)
	if err != nil {
		return nil, nil, fmt.Errorf("could not render server template %v: %w", file, err)
	}

	decoder := scheme.Codecs.UniversalDeserializer()

	obj, gvk, err := decoder.Decode(b.Bytes(), nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode rendered yaml into kubernetes object: %w", err)
	}

	return obj.(client.Object), gvk, nil
}

// renderAgentTemplate renders a template from e2e/templates/agent into a kubernetes object.
func renderAgentTemplate(file string, params any) (client.Object, *schema.GroupVersionKind, error) {
	b := &bytes.Buffer{}

	err := agentTemplates.ExecuteTemplate(b, file, params)
	if err != nil {
		return nil, nil, fmt.Errorf("could not render server template %v: %w", file, err)
	}

	decoder := scheme.Codecs.UniversalDeserializer()

	obj, gvk, err := decoder.Decode(b.Bytes(), nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode rendered yaml into kubernetes object: %w", err)
	}

	return obj.(client.Object), gvk, nil
}
