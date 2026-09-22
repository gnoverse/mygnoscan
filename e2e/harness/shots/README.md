# Capture fixtures

Real captures of live mainnet realms, taken with
[gnoshot](https://github.com/gnoverse/gnoshot):

```
gnoshot capture -out . -mode render https://gno.land/r/gov/dao
```

`shot-stub.mjs` serves these in place of a capture service, picking one by a
stable hash of the requested URL.

They are committed, and they are committed for one reason: the e2e fixture's
paths are synthetic. `gno.land/r/hub/core` exists on no chain, so a real capture
service answers nothing for any of them and the review screenshots would be
twenty pictures of a fallback tile — which proves the fallback works and nothing
else.

Two rungs each (`thumb` 320x180, `hero` 640x360), WebP, about 25 KB per realm.
Regenerate them only if the ladder's geometry changes; a fresh capture of the
same realm on a different day is churn in a binary file for no gain.
