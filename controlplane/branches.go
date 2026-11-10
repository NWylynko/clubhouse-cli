package controlplane

func TruncateBranchesAhead(names []string) (out []string, omitted int) {
	if len(names) > MaxBranchesAhead {
		omitted = len(names) - MaxBranchesAhead
		names = names[:MaxBranchesAhead]
	}
	out = make([]string, len(names))
	for i, n := range names {
		if len(n) > MaxBranchNameLen {
			n = n[:MaxBranchNameLen]
		}
		out[i] = n
	}
	return out, omitted
}
