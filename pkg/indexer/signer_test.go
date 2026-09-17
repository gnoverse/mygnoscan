package indexer

import "testing"

// Deriving who actually signed a transaction, from real mainnet content.
//
// A message names its *caller* — the account being acted for — which is the
// same whether that account signed for itself or a session key signed on its
// behalf. Only the signature distinguishes them, and gno's session feature
// exists precisely so they can differ.
//
// The fixtures are real transactions rather than synthesised bytes, because the
// failure this guards against is a derivation that looks right and is not: an
// earlier attempt used truncated sha256 instead of ripemd160 and produced valid
// bech32 addresses that matched nothing, which reads as "every transaction is
// session-signed". Self-signed controls are what catch that, so they come
// first.
func TestSignerAddress(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		caller   string
		wantSame bool
	}{
		{
			// A plain send: the signer must be its own from_address, or the
			// derivation is wrong.
			name:     "self-signed send",
			content:  `Tx{0A730A0D2F62616E6B2E4D736753656E6412620A28673167687A3661673972707530363266797A6B3973777670796C30726E3373336865646775793466122867313373393437356134396766743338787A3772326E793474636B7965726B7239726A75757873361A0C3130303030303075676E6F74120F08FCB97D12093130323875676E6F741A7E0A3A0A132F746D2E5075624B6579536563703235366B3112230A21028DFCCF9EEE95115092BB4CA74EDD7077A8B01FAA117A7156F6862837D2316492124028C8CF95BC6D358534E1ED1597D3B746BCFBF325BCC19580C42963D18933AE5301E1B464CF2BE9183337001855474F4A12A99132E112C2B3EA1AFBC9FF81B54B}`,
			caller:   "g1ghz6ag9rpu062fyzk9swvpyl0rn3s3hedguy4f",
			wantSame: true,
		},
		{
			name:     "self-signed call",
			content:  `Tx{0A90010A0A2F766D2E6D5F63616C6C1281010A2867316C3074647A6479787361796573746D386C7539357865686774633073383235707875353833672216676E6F2E6C616E642F722F676E6F737761702F676E732A07417070726F766532286731656D397334306E667277643261716E3979706A7637643978397A396338756B357578727A6139320A383538393030303030300A9A010A0A2F766D2E6D5F63616C6C128B010A2867316C3074647A6479787361796573746D386C753935786568677463307338323570787535383367221D676E6F2E6C616E642F722F676E6F737761702F676F762F7374616B65722A0844656C6567617465322867316C3074647A6479787361796573746D386C753935786568677463307338323570787535383367320A3835383930303030303032000A86010A0A2F766D2E6D5F63616C6C12780A2867316C3074647A6479787361796573746D386C7539357865686774633073383235707875353833672216676E6F2E6C616E642F722F676E6F737761702F676E732A07417070726F766532286731656D397334306E667277643261716E3979706A7637643978397A396338756B357578727A6139320130121208ACBCCB6B120B31313238313675676E6F741A7E0A3A0A132F746D2E5075624B6579536563703235366B3112230A210334F0077B06D234643FAF1133A28F3AE826FCD08A491837AD2908B54CF698FBE81240289C674ED2BE49AB9985F948C091DB1D4053FAD5E29A2A96C895AB9FA6FC3EBD05D3976777C286081AFAB0246271EA9E468AA53DAD4AEAB667EAB4D0980D4A54221B4578656375746564207468726F75676820676E6F737761702E696F}`,
			caller:   "g1l0tdzdyxsayestm8lu95xehgtc0s825pxu583g",
			wantSame: true,
		},
		{
			// Signed by a key registered via MsgCreateSession at block 115473,
			// whose allow_paths named the very realm this call targets.
			name:     "session-signed call",
			content:  `Tx{0A610A0A2F766D2E6D5F63616C6C12530A2867316D616E6672656434376B7A647565633932307A38387766723634796C6B736D646365646C66352222676E6F2E6C616E642F722F6D6F756C2F782F6461696C792F636F756E7465722F76302A03496E63121108C09FAB03120A323030303075676E6F741AA8010A3A0A132F746D2E5075624B6579536563703235366B3112230A2103E029A33FC2E636D270221CF4F9D53D51ACD1CF7B38D731F8B1A1FD42987D05D3124086FAE4674BF189AE707C11784E1C918F475F5D3C5124AB4DC3EC9C661A90C82E187E5110D7DBCFDA901FAB50121B50108135303A013E738BCB960A0F7ADF9DC41A2867313077347676386B6D35743077333832766C3571676E6D61356465747933666E73636637353338}`,
			caller:   "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
			wantSame: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SignerAddress(tt.content)
			if got == "" {
				t.Fatalf("no signer derived from %d bytes of content", len(tt.content))
			}
			if same := got == tt.caller; same != tt.wantSame {
				if tt.wantSame {
					t.Errorf("signer %s != caller %s; a self-signed transaction must derive to its own caller", got, tt.caller)
				} else {
					t.Errorf("signer %s == caller %s; this transaction was signed by a session key and must not look self-signed", got, tt.caller)
				}
			}
		})
	}
}

// Content that carries no key is a normal answer, not an error: content_raw is
// only selected on single-transaction queries, so list views have nothing to
// derive from and must not be reported as anonymous.
func TestSignerAddressWithoutAKey(t *testing.T) {
	for _, in := range []string{"", "Tx{}", "not hex", "deadbeef"} {
		if got := SignerAddress(in); got != "" {
			t.Errorf("SignerAddress(%q) = %q, want empty", in, got)
		}
	}
}
