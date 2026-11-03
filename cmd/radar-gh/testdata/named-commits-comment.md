/radar

Round 4 (review of `eb91a6c1085c`).

**"Policy-limit test covers only one bundle shape" (P2): fixed in `2dce474a24`.** `TestRolesStayWithinThePoliciesPerRoleLimit` builds each role with the real bundle constructors and the real managed policy. It counts the role's own attached policies, so a further attachment fails the test, including one added inside the role constructor:
- the API service (`sharedPolicies` + `podRoleSplit`) is checked at ≤ 10 for both shared-bundle shapes. The scoped shape is covered twice, once via `use_service_roles` and once via `use_new_pod_policy`; the unscoped shape once.
- the workers (`dedicatedPolicies` + `namedPodRoleSplit`) are pinned at exactly 10.

**"Resolve or explicitly accept legacy access from older roles" (P2): no code.** Roles without the new policy keep the legacy `queue-<ns>-*` reach over the regional names, including `CreateBucket` and bucket configuration. That reach is accepted for the transition, pending the security owner's sign-off, and is listed under Known limits in the PR body. On every role that writes or replays, the Denies from `eb91a6c108` already narrow it to object access in the deployment account. Retiring the legacy grants is tracked in [OPS-1854](https://tracker.example.com/OPS-1854) ("Retire the legacy queue-<namespace>-* grants"), a sub-issue of OPS-1829.
