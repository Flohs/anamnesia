// ownership.go decides what one Anamnesia install may do with a Postgres
// container, given which install created it.
//
// The container name, its volume and its port all live inside a config file,
// and that config file is selected by ANAMNESIA_HOME. So two homes whose
// config files resolve to the same container name are pointed at the same
// database, and neither knows about the other. A scratch home left on the
// defaults will therefore adopt a real install's container.
//
// Adopting it is survivable. What is not is the next step: the password
// reconcile exists to repair a config whose password drifted from its own
// container, and it repairs it by rewriting the role password. Run against
// someone else's container, that is a working install broken silently by a
// process that believed it was helping.
//
// So containers now carry the home that created them, and these rules decide
// the rest. They are pure on purpose: the docker plumbing around them is
// awkward to test, and the decision is the part that must not be wrong.
package main

import "fmt"

// containerOwnerLabel records the ANAMNESIA_HOME that created a container.
const containerOwnerLabel = "anamnesia.home"

// containerDecision is what this install may do with a container.
type containerDecision struct {
	mayUse           bool
	mayResetPassword bool
}

// containerDecision reports what ourHome may do with a container labelled
// ownerHome. An empty ownerHome means the container predates ownership
// labels, which every container created before this did.
//
// adopt is the operator saying "this unlabelled container is mine" — it
// unlocks the password reset for that case and nothing else. It deliberately
// cannot override a label naming someone else: a flag that seizes a container
// demonstrably in use by another install is the incident this prevents,
// wearing a different hat.
func decideContainer(ownerHome, ourHome string, adopt bool) (containerDecision, error) {
	switch {
	case ownerHome == ourHome:
		return containerDecision{mayUse: true, mayResetPassword: true}, nil

	case ownerHome != "":
		return containerDecision{}, fmt.Errorf(
			"this container was created by a different Anamnesia install.\n"+
				"  it belongs to: %s\n"+
				"  you are using: %s\n"+
				"Point `postgres.container` somewhere else, or set ANAMNESIA_HOME to the install that owns it.\n"+
				"Sharing one container between two homes means sharing one database, and the first\n"+
				"password reconcile would lock the other one out.",
			ownerHome, ourHome)

	case adopt:
		return containerDecision{mayUse: true, mayResetPassword: true}, nil

	default:
		// Usable, because refusing outright would break every install whose
		// container predates the label. Not rewritable, because that is the
		// step that does the damage.
		return containerDecision{mayUse: true}, nil
	}
}

// passwordResetRefused explains the one refusal a user is likely to meet:
// the right container, a password that does not match, and no label to prove
// whose it is.
func passwordResetRefused(container, ourHome string) error {
	return fmt.Errorf(
		"the database in container %q does not accept the password in this config,\n"+
			"and the container carries no record of which install created it.\n"+
			"Refusing to change its password: if it belongs to another install, doing so\n"+
			"would lock that one out of its own memory.\n"+
			"  this install: %s\n"+
			"If the container is genuinely yours, re-run with --adopt to take ownership.\n"+
			"If it is not, point `postgres.container` at a different name and run again.",
		container, ourHome)
}
