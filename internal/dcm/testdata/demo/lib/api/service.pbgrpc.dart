import 'dart:async';

Future<int> callService(int a) async {
  if (a > 0) {
    return a;
  }
  return 0;
}
