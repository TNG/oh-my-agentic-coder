// Package vm manages the one persistent Lima VM per host ("omac VM") that
// sessions attach to for Docker and, in a later step, Gradle. The VM is
// created, provisioned and switched between an online mode (provisioning,
// priming) and the default offline mode (nftables ruleset omac-vmguard
// loaded, the only mode sessions may attach to). All host-side state lives
// under one directory that is never granted to a sandbox.
//
// Every external effect (limactl, ssh, ssh-keygen, the Docker /version
// probe, image pulls) goes through a function type in Deps so the whole
// lifecycle runs without a VM in tests.
package vm
