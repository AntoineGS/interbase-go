-- Synthetic SQL fixture for scoped plan-alias resolution.
SELECT sub_q.*,
  (SELECT MIN(lkp_64.group_name)
   FROM (SELECT a.group_id, b.group_name, a.item_code, a.item_name
         FROM PRODUCT_ITEMS a
         LEFT JOIN PRODUCT_GROUPS b ON a.group_id = b.group_id
         WHERE b.is_active = 'Y') lkp_64
   WHERE lkp_64.item_code = sub_q.product_code),
  (SELECT MIN(lkp_29.color_name)
   FROM (SELECT a.color_id, a.color_name, b.class_id, b.class_name
         FROM COLOR_ATTRIBUTES a
         LEFT JOIN COLOR_CLASSES b ON b.class_id = a.class_id
         WHERE b.is_visible = 'Y') lkp_29
   WHERE lkp_29.color_id = sub_q.color_id),
  (SELECT MIN(lkp_1.brand_name)
   FROM (SELECT brand_id, brand_name FROM BRANDS) lkp_1
   WHERE lkp_1.brand_id = sub_q.brand_id),
  (SELECT MIN(lkp_5.vendor_name)
   FROM (SELECT vendor_id, vendor_name FROM VENDORS) lkp_5
   WHERE lkp_5.vendor_id = sub_q.vendor_id),
  (SELECT MIN(lkp_6.account_name)
   FROM (SELECT vendor_id, account_name FROM VENDOR_ACCOUNTS) lkp_6
   WHERE lkp_6.vendor_id = sub_q.vendor_id),
  (SELECT MIN(lkp_12.category_name)
   FROM (SELECT category_id, category_name FROM CATEGORIES) lkp_12
   WHERE lkp_12.category_id = sub_q.category_id),
  (SELECT MIN(lkp_41.currency_code)
   FROM (SELECT currency_id, currency_code FROM CURRENCIES) lkp_41
   WHERE lkp_41.currency_id = sub_q.currency_id),
  (SELECT MIN(lkp_55.product_name)
   FROM (SELECT product_id, product_name FROM PRODUCTS) lkp_55
   WHERE lkp_55.product_id = sub_q.product_id),
  (SELECT MIN(lkp_56.stock_status)
   FROM (SELECT product_id, stock_status FROM STOCK_LEVELS) lkp_56
   WHERE lkp_56.product_id = sub_q.product_id),
  (SELECT SUM(x.quantity_on_hand)
   FROM STOCK_LEVELS x
   WHERE x.product_id = sub_q.product_id
     AND x.location_id = sub_q.location_id)
FROM (SELECT a.product_id, a.product_code, a.product_group_id, a.vendor_id,
             a.category_id, a.brand_id, a.currency_id, a.color_id,
             a.location_id, b.account_id, b.account_status, c.vendor_name,
             d.group_name, e.brand_name, f.category_name, g.currency_code,
             h.color_name, i.class_name, j.quantity_on_hand, k.location_name,
             l.unit_name
      FROM PRODUCTS a
      LEFT JOIN VENDOR_ACCOUNTS b ON b.vendor_id = a.vendor_id
      LEFT JOIN VENDORS c ON c.vendor_id = b.vendor_id
      LEFT JOIN PRODUCT_GROUPS d ON d.group_id = a.product_group_id
      LEFT JOIN BRANDS e ON e.brand_id = a.brand_id
      LEFT JOIN CATEGORIES f ON f.category_id = a.category_id
      LEFT JOIN CURRENCIES g ON g.currency_id = a.currency_id
      LEFT JOIN COLORS h ON h.color_id = a.color_id
      LEFT JOIN COLOR_CLASSES i ON i.class_id = h.class_id
      LEFT JOIN STOCK_LEVELS j ON j.product_id = a.product_id
      LEFT JOIN LOCATIONS k ON k.location_id = a.location_id
      LEFT JOIN UNIT_TYPES l ON l.unit_type_id = a.unit_type_id
      /* Keep multiple joins to exercise source-scope analysis. */
      -- Optional source deliberately remains commented out:
      -- LEFT JOIN PRODUCT_NOTES m ON m.product_id = a.product_id
      WHERE a.product_id IN
        (SELECT y.product_id FROM PRODUCT_ITEMS y
         WHERE y.group_id = a.product_group_id)) sub_q
LEFT JOIN (SELECT DISTINCT product_id FROM PRODUCT_ATTRIBUTES) lkp_3
  ON lkp_3.product_id = sub_q.product_id
ROWS 200 TO 400
