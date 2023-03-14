package clusterSecret

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func IsClusterSecret(object metav1.Object) bool {
	secretType, ok := object.GetLabels()["argocd.argoproj.io/secret-type"]
	if !ok {
		return false
	}

	return secretType == "cluster"
}
