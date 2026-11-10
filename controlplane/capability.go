package controlplane

import "github.com/nwylynko/clubhouse-cli/detect"

func CapFromProfile(p detect.Profile) *MemberCapability {
	s := p.DefaultStack()
	return &MemberCapability{
		Runtime: s.Runtime,
		Version: s.Version,
		PkgMgr:  s.PkgMgr,
		Score:   p.Score,
	}
}

func LocalGeneration(cursorsPath string, load func(string) struct{ Generation int }) int {
	if load == nil {
		return 0
	}
	return load(cursorsPath).Generation
}
