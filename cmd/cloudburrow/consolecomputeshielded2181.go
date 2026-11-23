package main

import (
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Compute Engine console parity, Shielded VM (#2181): Create an instance's
// Security > Shielded VM, the instance's Details page Shielded VM group,
// and Edit's Shielded VM section, which calls
// instances.updateShieldedInstanceConfig while the VM is stopped.
//
// Google documents the console's three settings in
// docs.cloud.google.com/compute/shielded-vm/docs/modifying-shielded-vm and
// shielded-vm/docs/quickstart (read 2026-11-23): stop the VM, click Edit,
// and in the Shielded VM section toggle Turn on Secure Boot, Turn on vTPM
// and Turn on Integrity Monitoring, then Save. Compute Engine does not
// enable Secure Boot by default and enables the vTPM and integrity
// monitoring by default (shielded-vm/docs/shielded-vm).

// shieldedSection is Google's heading for the settings, a subsection of
// Create an instance's Security pane and a section of Edit.
const shieldedSection = "Shielded VM"

// shieldedNote is the line drawn under Create an instance's Shielded VM,
// over its three checkboxes: Google's own recommendation, verbatim from
// compute/docs/instances/modifying-shielded-vm (read 2026-11-23), as Secure
// Boot is the one setting off by default.
const shieldedNote = "If possible, Google recommends enabling Secure Boot."

// shieldedRunningNote is Edit's reason the settings are drawn disabled
// while the VM is not stopped: updateShieldedInstanceConfig refuses the
// change on a VM that is not TERMINATED (modifying-shielded-vm: "After the
// instance stops, click Edit"). Google documents no console text for this
// state: the wording is unverified, kept short and neutral.
const shieldedRunningNote = "Stop the VM to change these settings."

// shieldedKeys are the API's shieldedInstanceConfig fields, in Google's
// order. The labels are the console's (modifying-shielded-vm); each help
// line is the first sentence of the setting's section in
// compute/shielded-vm/docs/shielded-vm (read 2026-11-23), verbatim.
var shieldedKeys = []struct{ name, label, help, def string }{
	{"enableSecureBoot", "Turn on Secure Boot",
		"Secure Boot helps ensure that the system only runs authentic software by verifying the digital signature of all boot components, and halting the boot process if signature verification fails.", "false"},
	{"enableVtpm", "Turn on vTPM",
		"A vTPM is a virtualized trusted platform module, which is a specialized computer chip you can use to protect objects, like keys and certificates, that you use to authenticate access to your system.", "true"},
	{"enableIntegrityMonitoring", "Turn on Integrity Monitoring",
		"Integrity monitoring helps you understand and make decisions about the state of your VM instances.", "true"},
}

// shieldedCreateFields are Create an instance's Security > Shielded VM.
// Integrity Monitoring requires the vTPM, as the API refuses it without:
// checking it turns the vTPM on and holds it there.
func shieldedCreateFields(section string) []console.Field {
	out := make([]console.Field, 0, len(shieldedKeys))
	for i, k := range shieldedKeys {
		f := console.Field{Name: k.name, Label: k.label, Type: "checkbox", Section: section, Default: k.def, Help: k.help, Subsection: shieldedSection}
		if i == 0 {
			f.SubsectionNote = shieldedNote
		}
		if k.name == "enableIntegrityMonitoring" {
			f.Requires = []string{"enableVtpm"}
		}
		out = append(out, f)
	}
	return out
}

// withShieldedVM puts Shielded VM first in the Security pane, over the
// service account and access scopes, as Google's Create an instance has it.
func withShieldedVM(section string, fields []console.Field) []console.Field {
	for i, f := range fields {
		if f.Section == section {
			return append(append(append([]console.Field{}, fields[:i]...), shieldedCreateFields(section)...), fields[i:]...)
		}
	}
	return append(fields, shieldedCreateFields(section)...)
}

// shieldedCreateBody is the instance's shieldedInstanceConfig from the
// form; a form that sent none of the three keeps the API's defaults.
func shieldedCreateBody(values map[string]string) map[string]any {
	cfg := map[string]any{}
	for _, k := range shieldedKeys {
		if v, ok := values[k.name]; ok {
			cfg[k.name] = v == "true"
		}
	}
	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

// shieldedConfig is the instance's settings, the API's defaults where it
// holds none.
func shieldedConfig(o map[string]any) map[string]bool {
	cfg := vmMap(o, "shieldedInstanceConfig")
	out := map[string]bool{}
	for _, k := range shieldedKeys {
		v, ok := cfg[k.name].(bool)
		if !ok {
			v = k.def == "true"
		}
		out[k.name] = v
	}
	return out
}

// shieldedGroup is the Details page's Shielded VM: Secure Boot, vTPM and
// Integrity Monitoring, each On or Off.
func shieldedGroup(o map[string]any) console.PropertyGroup {
	cfg := shieldedConfig(o)
	onOff := map[bool]string{true: "On", false: "Off"}
	return console.PropertyGroup{Heading: shieldedSection, Properties: []console.Property{
		{Label: "Secure Boot", Value: onOff[cfg["enableSecureBoot"]]},
		{Label: "vTPM", Value: onOff[cfg["enableVtpm"]]},
		{Label: "Integrity Monitoring", Value: onOff[cfg["enableIntegrityMonitoring"]]},
	}}
}

// shieldedEditFields are Edit's Shielded VM section: the instance's
// settings, changeable only while it is stopped; otherwise drawn disabled
// with the reason, and not sent.
func shieldedEditFields(o map[string]any) []console.Field {
	cfg := shieldedConfig(o)
	stopped := vpcStr(o, "status") == "TERMINATED"
	out := make([]console.Field, 0, len(shieldedKeys))
	for i, k := range shieldedKeys {
		f := console.Field{Name: k.name, Label: k.label, Type: "checkbox", Section: shieldedSection,
			Default: strconv.FormatBool(cfg[k.name]), Help: k.help}
		if k.name == "enableIntegrityMonitoring" {
			f.Requires = []string{"enableVtpm"}
		}
		if !stopped {
			// Disabled, the reason said once, over the first.
			f.Immutable, f.Help = true, ""
			if i == 0 {
				f.Help = shieldedRunningNote
			}
		}
		out = append(out, f)
	}
	return out
}

// shieldedEditBody is the updateShieldedInstanceConfig body for what Edit
// changed, nil when nothing did (or the fields were not sent, as while the
// VM runs).
func shieldedEditBody(o map[string]any, values map[string]string) map[string]any {
	cfg := shieldedConfig(o)
	body := map[string]any{}
	for _, k := range shieldedKeys {
		v, ok := values[k.name]
		if !ok {
			continue
		}
		if want := v == "true"; want != cfg[k.name] {
			body[k.name] = want
		}
	}
	if len(body) == 0 {
		return nil
	}
	return body
}
