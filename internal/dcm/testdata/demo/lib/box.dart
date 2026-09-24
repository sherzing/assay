class Box {
  Box();
  Box.named(this._v);
  Box.fallback(int? v) : _v = v ?? 0;
  int _v = 0;

  int get value => _v > 2 ? _v : 0;
  set value(int v) {
    if (v < 0) {
      _v = 0;
    } else {
      _v = v;
    }
  }

  int work(int n) {
    int inner(int k) {
      if (k > 3) return k * 2;
      return k;
    }
    var s = 0;
    for (var i = 0; i < n; i++) {
      s += inner(i);
    }
    return s;
  }

  void clear() {}
}

extension on int {
  int triple() => this * 3;
}
