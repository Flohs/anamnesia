package main

import (
	"strings"
	"testing"
)

// A container carries the home that created it. These rules decide what a
// different home may do with it, and they exist because a scratch
// ANAMNESIA_HOME that resolved to the default container name adopted a real
// install's database and rewrote its password to match its own config.

func TestOurOwnContainerIsFullyUsable(t *testing.T) {
	d, err := decideContainer("/home/floh/.anamnesia", "/home/floh/.anamnesia", false)
	if err != nil {
		t.Fatalf("our own container was refused: %v", err)
	}
	if !d.mayUse || !d.mayResetPassword {
		t.Errorf("decision = %+v, want full use of a container we created", d)
	}
}

func TestAnotherHomesContainerIsRefusedOutright(t *testing.T) {
	_, err := decideContainer("/home/floh/.anamnesia", "/tmp/scratch/anamnesia-dev", false)
	if err == nil {
		t.Fatal("a container owned by another home was accepted")
	}
	// The message has to name both homes, or the reader cannot tell which
	// of their configs is wrong.
	for _, want := range []string{"/home/floh/.anamnesia", "/tmp/scratch/anamnesia-dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestAnotherHomesContainerIsRefusedEvenWithAdopt(t *testing.T) {
	// --adopt exists for unlabelled containers from before ownership was
	// recorded. It must never be a way to seize one that is demonstrably
	// someone else's.
	if _, err := decideContainer("/home/floh/.anamnesia", "/tmp/scratch/dev", true); err == nil {
		t.Fatal("--adopt overrode a known, different owner")
	}
}

func TestAnUnlabelledContainerIsUsableButNotRewritable(t *testing.T) {
	// Containers created before ownership labels exist on real installs.
	// Using one is fine; silently changing its password is what caused the
	// incident, so that needs an explicit decision.
	d, err := decideContainer("", "/home/floh/.anamnesia", false)
	if err != nil {
		t.Fatalf("an unlabelled container was refused outright: %v", err)
	}
	if !d.mayUse {
		t.Error("an unlabelled container should still be usable")
	}
	if d.mayResetPassword {
		t.Error("an unlabelled container's password must not be rewritten without --adopt")
	}
}

func TestAdoptPermitsRewritingAnUnlabelledContainer(t *testing.T) {
	d, err := decideContainer("", "/home/floh/.anamnesia", true)
	if err != nil {
		t.Fatalf("--adopt was refused for an unlabelled container: %v", err)
	}
	if !d.mayResetPassword {
		t.Error("--adopt should permit the password reset it exists for")
	}
}

func TestRefusalToRewriteNamesTheWayForward(t *testing.T) {
	// The failure a user actually hits: right container, wrong password,
	// no label. The error must say what to do rather than just refusing.
	err := passwordResetRefused("anamnesia-postgres", "/home/floh/.anamnesia")
	for _, want := range []string{"anamnesia-postgres", "--adopt", "postgres.container"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
