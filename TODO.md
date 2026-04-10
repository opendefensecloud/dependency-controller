  There are aspects that will need iteration once we run against a real kcp instance — particularly the
  dynamic controller registration (controller-runtime doesn't natively support stopping controllers, so the
  DependencyRule deletion cleanup path may need refinement).
