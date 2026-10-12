package plugin

// multiDirSHA256 is the SHA-256 of the embedded plugin (assets/omac-multidir.ts).
// TestMultiDirHashesUpToDate fails when the plugin changes and this is not
// updated, as a reminder to also move the old value into previousMultiDirSHA256.
const multiDirSHA256 = "8e7a3e0f04b4c082b64040579a56f70e0f63fc6f9288db6db5acf79795fb3fd3"

// previousMultiDirSHA256 lists the SHA-256 of every earlier omac-multidir.ts
// committed to this repo. A plugin file matching one of them is an unedited
// copy written by an older omac, so installs may replace it without --force
// (issue #360: a v0.9.0 copy sends no control token and gets 401 from v0.10.0).
//
// Regenerate with:
//
//	for c in $(git log --all --format=%H -- internal/plugin/assets/omac-multidir.ts); do
//	  git show $c:internal/plugin/assets/omac-multidir.ts | sha256sum; done | sort -u
var previousMultiDirSHA256 = map[string]bool{
	"01834d4eac45fd5c3ea68cd832a36dfa61fa4fb3ec7711583f5765195aae8844": true,
	"2d8c29c80b7b4afc1820150e673af333eae5669059ad73af7c3cd64531fb0783": true,
	"336341b54e105bef411a166a6d2e9129d290c9a09030d73e7e1e19d3b920e2cd": true,
	"436e85e2cdfb3dddbbaa3f75d84d6095c29d5852bc4a5c96d119e6322111515a": true,
	"4ad168184f0d623f69afa46cf28c9de6d0ef1f0a6df0cc050008d000b1ed9965": true,
	"5d8a4e502a5b7f426c8878c5a8765e004e098be72ab9c4f0241c0bb0d6d88a3b": true,
	"5f74740c0a37e63827cde14c415386cc8f661960a125f477a69cbee0c78b6924": true,
	"682cb2f89cef84ba89a68a02b58bdf33c563ec3947915eca0f79f5017969b5d5": true,
	"6c11a025f6ad4aa9369a832b275fb62a57b880016f6e61392e019bdebbc19e59": true,
	"8484b911b02f3e4dde4a5ce1ec6ea3725cf89d2a1940a76fd460976f00a58325": true,
	"b21372b67d0b34dac48ca97ccc57ed1505244e52f9a716deb5eff20cfb094854": true,
	"caaa8cec5de3f8d1b91b0ca1c77cfd8e87fef22bf1facd9f08b163f8c4724a04": true,
	"da5995f7baf23196d564ead4ad4459ccb37ef847c8a182dfc3870761d87ffb59": true,
	"e2f65fcf5ce30288cb9ed4e53a85f0e11b6976baa58fe1c9533dd00a41dfb8b6": true,
	"e9fffa8b48b716d695a6a713e6a2c4389aca25b89962316167b1ab0ceccb352b": true,
	"f7727212e0eea1f406ff58f18f200437e753cace782e4d0edd572ef582bfd080": true,
}
