package config

import "testing"

func TestNormalizeTransactionPolicy(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "", want: TransactionPolicyECDSA},
		{input: " ECDSA ", want: TransactionPolicyECDSA},
		{input: "HYBRID-MLDSA65", want: TransactionPolicyHybridMLDSA65},
		{input: " PQC-OPT-IN-MLDSA65 ", want: TransactionPolicyPQCOptInMLDSA65},
		{input: "auto", wantErr: true},
	}
	for _, test := range tests {
		got, err := NormalizeTransactionPolicy(test.input)
		if test.wantErr {
			if err == nil {
				t.Errorf("NormalizeTransactionPolicy(%q) unexpectedly succeeded", test.input)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("NormalizeTransactionPolicy(%q) = (%q, %v), want %q", test.input, got, err, test.want)
		}
	}
}
