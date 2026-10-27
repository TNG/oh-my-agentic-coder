// Package vm manages the one persistent Lima VM per host ("omac VM") in
// which sessions run Docker, Gradle and tests through `omac vm exec`. The VM is
// created, provisioned and switched between an online mode (provisioning,
// priming) and the default offline mode (nftables ruleset omac-vmguard
// loaded, the only mode sessions may attach to). All host-side state lives
// under one directory that is never granted to a sandbox.
//
// Every external effect (limactl, ssh, ssh-keygen, image pulls) goes
// through a function type in Deps so the whole lifecycle runs without a VM
// in tests.
package vm
