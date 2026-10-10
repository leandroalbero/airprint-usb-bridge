package ql

func probeModel(d Device, p *Profile, t Timeouts) (Status, bool, error) {
	st, err := requestStatusTimeouts(d, t)
	if err != nil {
		return Status{}, false, err
	}
	if p == nil {
		return st, false, nil
	}
	ok := st.ModelByte3 == p.StatusModelByte3 && st.ModelByte4 == p.StatusModelByte4
	return st, ok, nil
}
