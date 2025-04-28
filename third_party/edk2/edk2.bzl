filegroup(
    name = "all",
    srcs = glob(
        ["**"],
        exclude = [
            "CryptoPkg/Library/OpensslLib/openssl/boringssl/fuzz/*_corpus/**",
            "CryptoPkg/Library/OpensslLib/openssl/fuzz/corpora/**",
        ],
    ),
    visibility = ["//visibility:public"],
)
