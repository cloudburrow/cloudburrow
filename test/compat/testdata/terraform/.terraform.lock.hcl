# The dependency lock for the hashicorp/google provider the Terraform and
# OpenTofu compat tests use (#694). The tests copy it into each module's
# directory before init, so init installs 8.12.0 only if the package matches a
# hash here and fails otherwise. One file serves both tools: Terraform reads
# the registry.terraform.io entry and OpenTofu the registry.opentofu.org one,
# whose packages and hashes differ. Regenerate both entries on a bump, from a
# directory whose main.tf pins the same exact version:
#
#   terraform providers lock -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64 -platform=linux_arm64
#   tofu providers lock -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64 -platform=linux_arm64
#
# then join the two provider blocks here, and change googleProviderVersion in
# test/compat/realtools_test.go and hashicorpGoogleProvider in dependencies.json.

provider "registry.terraform.io/hashicorp/google" {
  version     = "8.12.0"
  constraints = "8.12.0"
  hashes = [
    "h1:9d+FdGigXECjhHgs8RcW5JhsTTXVNga0X1Pd4O4QHjY=",
    "h1:Lx9hIIVVH8pAcDPS2fsJmRUeR/tq6+StmteW7VjORgk=",
    "h1:es4kOf5hndCahoF3bLHmsLKX93JpkeuThvKQ4Pnm6m8=",
    "h1:xKyuRUAQu+81b420c1D1aC3Gjht4g+fUJ4qi3x8B4xU=",
    "zh:0b486637c4f4a2ff260dcf98e0ab74aa2655884c27df909fc79de9c2829ea6ef",
    "zh:0b9c04d4bb269389d799f6b871877172d13cd059571768c6c36b28cf297b6fba",
    "zh:0c63d340b3fda4156bccbd4f02e8ef7d320bea75bd1706573a90851e00129c47",
    "zh:20371948b3d7f0469f2ad99608d9626a816127c9eb1b4cd007f09e1f68878728",
    "zh:2a6d9c60699366894da043e604ebe5fee9246bb01bef73ebd100d3dbaf3e5e34",
    "zh:47aa133c201e1edd2d1eb789f2ef4ccc769fdb49d5000b042280c2ac37acf368",
    "zh:58e2910ee0625ecd352c2e55050ed35771ed7860dd1e738ee4b53eab695a2ee8",
    "zh:84a44faf0dc811e48a8590e244c02ffff4a6a10095a4bc869979e7b0e94ae46d",
    "zh:b8f42d7d79776b54d37c93b51a3f10ae45425fb91fb182bcb2a966e1bd52bdad",
    "zh:eddf30f53a99381388ff25702ed0c727586deb8c502cd5af395dc98afb217a49",
    "zh:f569b65999264a9416862bca5cd2a6177d94ccb0424f3a4ef424428912b9cb3c",
    "zh:f79b40a9d29763535baed6de1cd0d42960059916a1723f8fb4af8a2801dd2f4c",
  ]
}

provider "registry.opentofu.org/hashicorp/google" {
  version     = "8.12.0"
  constraints = "8.12.0"
  hashes = [
    "h1:/uOUGLXWxUIu7H8yyzNXbzA8JhNWKNTrO9UkGrMNYqg=",
    "h1:04GEECjKgtNHUTrC/X7hfJtHVMsDFEekOuLJ6RFw/nw=",
    "h1:5Id0vzlRWYNUQumoNe2KgIlnxlLib4TgNNkk069BhWE=",
    "h1:8Lqu7V4du3l43jzqDqLbQEr34e7C4Haq3nI+znkeHtY=",
    "h1:Cdz/OBoQimTddlOyIQQ/rUoj4gtMIys83srv8rdS4Ww=",
    "h1:HhqxuoNLBfSLf7GDsCmD45Q+LcCjhjvU/r719IXRXq0=",
    "h1:M4KLSR1b7ezu9XwGKGr48PZ6YefB9/jRgvUsOW2i9aQ=",
    "h1:OCuGWEkRWj9fg06ilKpZiqqlyXrdVxSOVstASjhJoRY=",
    "h1:X7LqEVdWWRh/usYRqDPv1ZPxJIT4+tgC7i6sAKaq+7w=",
    "h1:cwJw5kj3XtQ7OMyxRWhjCibOG39dSTngvbcKUgviWSI=",
    "h1:fDGjZPzSkUJm8y9OSpCMQIxYMLBb0oUklknrbIkwzWc=",
    "h1:hB3JVMORvol27lIuR6oV+eBzzEdSrOoNrfx1/6Ql+4U=",
    "h1:jXiz6/rkxZlzZ5itXKyqC8JFVvj3U9tvo4KYGe8SMso=",
    "h1:mtCGDtxLrcat+MZX0pM8dzBBdF8ohv1aqegJAPVpxWQ=",
    "h1:titZh+2WzkA1y3kCrl7u8IGu7KR5p6Feh3ou+8NJUFc=",
    "zh:058321bf3835869d22307bbabb4ebdc6a884ebe954e05f747e72e71710d64b75",
    "zh:143130745edfd449f7ccbb0844915d50094d6644f1ff3c7d5b4dc699e33ad6f4",
    "zh:3d9eee56fd44148a429baee37fb6f631f5cba86b1c2f3072da9bcc2d75f7eb5c",
    "zh:4baa98bc4aa846f003a6eeab1402ce9c85f5f0c68d4ae80c3771a756a536e0fd",
    "zh:4ef491d9753f5633aa5114d1c6b35e79e5c3bcd78a084c01360100cd528a3cf3",
    "zh:688610b98cbd202d4417a742d82b71edda4708c3bdb498c3eb0389cfa6efca22",
    "zh:9032da7445e49894e1177d21faaab896dab291bffb5d87b59e511ee4e87b1063",
    "zh:a23af6184862164b07035cac3e1b2a342206904b7ff9c1160c6937c9c50565e9",
    "zh:a6f8d59de59971655ddcc098d1e24b1d1a1bbdbe3f11502c2e749a57f4a3273d",
    "zh:c56ee8ff838446f2fea6de4676e219919c1771fef64d8b70f0b1486cfb0c4e7d",
    "zh:c6a39a9e932d4de72634b65ce0db8de476bf459ecaadfa9acbd4e3ea37a59463",
    "zh:cfca6a5d76762e45551cbab0efc77c8d97590ae2eb874bacba8d36ba44efd64a",
    "zh:d0a7ca1247511a79b24e1a57232566e61c128e8db66336b60056776a05aade87",
    "zh:e1b156dd380531236fb1abd3db5d485bb8a9c3dd5316b52e3969c0c196e2be5e",
    "zh:f76dcb75adbd08a73bd62a469e5e9db602d6e7873c4c5743c444261608a34033",
  ]
}
