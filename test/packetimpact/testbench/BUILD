load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_visibility = ["//test/packetimpact:__subpackages__"],
    licenses = ["notice"],
)

go_library(
    name = "testbench",
    srcs = [
        "connections.go",
        "dut.go",
        "dut_client.go",
        "layers.go",
        "rawsockets.go",
        "testbench.go",
    ],
    deps = [
        "//pkg/tcpip",
        "//pkg/tcpip/buffer",
        "//pkg/tcpip/header",
        "//pkg/tcpip/seqnum",
        "//pkg/usermem",
        "//test/packetimpact/proto:posix_server_go_proto",
        "@com_github_google_go-cmp//cmp:go_default_library",
        "@com_github_google_go-cmp//cmp/cmpopts:go_default_library",
        "@com_github_mohae_deepcopy//:go_default_library",
        "@org_golang_google_grpc//:go_default_library",
        "@org_golang_google_grpc//keepalive:go_default_library",
        "@org_golang_x_sys//unix:go_default_library",
        "@org_uber_go_multierr//:go_default_library",
    ],
)

go_test(
    name = "testbench_test",
    size = "small",
    srcs = ["layers_test.go"],
    library = ":testbench",
    deps = [
        "//pkg/tcpip",
        "@com_github_mohae_deepcopy//:go_default_library",
    ],
)
