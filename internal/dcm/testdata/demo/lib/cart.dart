import 'dart:math';

import 'pricing.dart';

dynamic globalScratch;

class Cart {
  // quality:false-positive the bag is genuinely heterogeneous by design
  dynamic bag = {};
  dynamic extra;
  final List<int> prices = [];
  String? label;

  int total(List<int> items, bool applyTax, int retries) {
    var sum = 0;
    for (final p in items) {
      if (p > 0) {
        if (p > 1000) {
          sum += p - 50;
        } else {
          sum += p;
        }
      }
    }
    if (applyTax) {
      sum = applyTaxTo(sum);
    }
    return sum;
  }

  int discounted(int amount) {
    final result = amount - 42;
    return result;
  }

  void addAll(List<int> items) {
    items.map((i) => i * 2);
    prices.addAll(items);
  }

  void clear() {}

  String shout() {
    return label!.toUpperCase() + Random().nextInt(7).toString();
  }

  int copyTotal(List<int> items, bool applyTax, int retries) {
    var sum = 0;
    for (final p in items) {
      if (p > 0) {
        if (p > 1000) {
          sum += p - 50;
        } else {
          sum += p;
        }
      }
    }
    if (applyTax) {
      sum = applyTaxTo(sum);
    }
    return sum;
  }

  int neverCalled() => 7;
}
