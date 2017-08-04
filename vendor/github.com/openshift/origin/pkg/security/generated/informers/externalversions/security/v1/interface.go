// This file was automatically generated by informer-gen

package v1

import (
	internalinterfaces "github.com/openshift/origin/pkg/security/generated/informers/externalversions/internalinterfaces"
)

// Interface provides access to all the informers in this group version.
type Interface interface {
	// SecurityContextConstraints returns a SecurityContextConstraintsInformer.
	SecurityContextConstraints() SecurityContextConstraintsInformer
}

type version struct {
	internalinterfaces.SharedInformerFactory
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory) Interface {
	return &version{f}
}

// SecurityContextConstraints returns a SecurityContextConstraintsInformer.
func (v *version) SecurityContextConstraints() SecurityContextConstraintsInformer {
	return &securityContextConstraintsInformer{factory: v.SharedInformerFactory}
}
