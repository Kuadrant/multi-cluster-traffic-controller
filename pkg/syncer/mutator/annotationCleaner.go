package mutator

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/metadata"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/status"
)

type AnnotationCleaner struct {
}

func (m *AnnotationCleaner) GetName() string {
	return "Annotation Cleaner"
}

func (m *AnnotationCleaner) Mutate(cfg syncer.MutatorConfig, obj *unstructured.Unstructured) error {
	annotationPrefixes := []string{
		JSONPatchAnnotationPrefix,
		syncer.MCTC_SYNC_ANNOTATION_PREFIX,
		status.SyncerClusterStatusAnnotationPrefix,
		"kubectl.kubernetes.io/last-applied-configuration",
	}

	for _, annotationPrefix := range annotationPrefixes {
		metadata.RemoveAnnotationsByPrefix(obj, annotationPrefix)
	}

	return nil
}
