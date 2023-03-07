package mutator

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/metadata"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

const (
	JSONPatchAnnotationPrefix = "mctc-syncer-patch/"
)

type JSONPatch struct {
}

func (m *JSONPatch) GetName() string {
	return "JSON Patch"
}

func (m *JSONPatch) Mutate(cfg syncer.MutatorConfig, obj *unstructured.Unstructured) error {
	patchString := metadata.GetAnnotation(obj, JSONPatchAnnotationPrefix+cfg.ClusterID)
	if patchString == "" {
		return fmt.Errorf("no patch found for sync target '%v' on object: %v/%v", cfg.ClusterID, obj.GetNamespace(), obj.GetName())
	}

	cfg.Logger.Info("got patch", "string", patchString)
	patch, err := jsonpatch.DecodePatch([]byte(patchString))
	if err != nil {
		return fmt.Errorf("error decoding patch: %v", err.Error())
	}
	objBytes, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	objBytes, err = patch.Apply(objBytes)
	if err != nil {
		return err
	}

	err = json.Unmarshal(objBytes, obj)
	if err != nil {
		return err
	}
	return nil
}
