# Not transpiled to slayer

- measure "net_line_revenue": renamed to "net_line_revenue_measure"; columns and measures share one namespace in a slayer model, and "net_line_revenue" names a column on "fct_order_lines"
- measure "quantity": renamed to "quantity_measure"; columns and measures share one namespace in a slayer model, and "quantity" names a column on "fct_order_lines"
- metric "aov": renamed to "aov_measure"; columns and measures share one namespace in a slayer model, and "aov" names a column on "fct_orders"
- relationship fct_orders -> dim_customer: not emitted to slayer at all: model "fct_orders" already joins "dim_customer", and a slayer model can declare at most one join per target model
