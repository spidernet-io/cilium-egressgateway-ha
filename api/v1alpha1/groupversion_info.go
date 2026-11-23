// Package v1alpha1 contains the v1alpha1 API of the egressgateway-ha.spidernet.io group.
// +kubebuilder:object:generate=true
// +groupName=egressgateway-ha.spidernet.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "egressgateway-ha.spidernet.io", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)
