---
title: SO Line
audience: user
module: sales
order: 2
---

An SO Line is one item on a Sales Order — an item, a quantity, and a
price. A Sales Order normally has several of these; together they make
up what the customer is actually buying.

## When to use it

You will usually add these from inside the Sales Order form's Lines
section, not as a standalone record — but SO Line is also independently
listable and importable (for example, via CSV) when that is more
convenient for bulk order entry.

## Adding a line

1. From a Sales Order's form, use the Lines section's add action (or go
   to **SO Line** and choose **New**, then pick the Sales Order).
2. Choose the **Item** being sold.
3. Enter the **Quantity** and **Unit Price**.
4. Enter the **Line Total** yourself — the system does not multiply
   quantity by unit price for you.
5. Save. Re-open the Sales Order form afterward and its Total will have
   picked up this line automatically.

## Rules to know

- Quantity, Unit Price, and Line Total cannot be negative.
- Sales Order and Item are both required — a line with no order or no
  item to reference is rejected.
- Line Total is a plain number you enter, not a computed field. If you
  change the quantity or price after the fact, update Line Total to
  match yourself.

## What it connects to

Every SO Line belongs to one **Sales Order** and names one **Item**. The parent Sales Order's Lines section sums every
line's Line Total into the order's own Total field each time the form is
opened.
