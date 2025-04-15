TargetInfo = provider(
    "A simple provider to hold target information.",
    fields = {
        "value": "The value of the target information.",
    },
)

def _target_info_impl(ctx):
    return [
        TargetInfo(
            value = ctx.attr.value,
        ),
    ]

target_info = rule(
    implementation = _target_info_impl,
    attrs = {
        "value": attr.string(
            mandatory = True,
        ),
    },
    doc = "A simple rule to determine a target information based on selects.",
)
