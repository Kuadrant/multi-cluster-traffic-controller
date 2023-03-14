package conditions

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
)

const (
	ConditionTypeReady string = "Ready"
)

var Clock clock.Clock = clock.RealClock{}

func HasCondition(existingConditions []metav1.Condition, c metav1.Condition) bool {
	for _, cond := range existingConditions {
		if c.Type == cond.Type && c.Status == cond.Status {
			return true
		}
	}
	return false
}

func GetConditionByType(conditions []metav1.Condition, cType string) *metav1.Condition {
	for _, cond := range conditions {
		if cond.Type == cType {
			return &cond
		}
	}
	return nil
}

func SetCondition(conditions []metav1.Condition, generation int64, conditionType string, status metav1.ConditionStatus, reason, message string) []metav1.Condition {
	newCondition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	}

	nowTime := metav1.NewTime(Clock.Now())
	newCondition.LastTransitionTime = nowTime

	// Search through existing conditions
	for idx, cond := range conditions {
		if cond.Type != conditionType {
			continue
		}

		if cond.Status == status {
			newCondition.LastTransitionTime = cond.LastTransitionTime
		}
		// Overwrite the existing condition
		conditions[idx] = newCondition
		return conditions
	}

	// No existing condition, so add a new one
	conditions = append(conditions, newCondition)
	return conditions
}
