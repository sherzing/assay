int applyTaxTo(int amount) {
  if (amount > 500) {
    return amount + amount * 15 ~/ 100;
  }
  return amount + amount * 5 ~/ 100;
}

class TaxTable {
  static const int low = 5;
  static const int high = 15;
  static const int unusedBand = 25;
}
