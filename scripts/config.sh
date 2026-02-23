cat > /root/.my-hosto.cnf << 'EOF'
[client]
user=control
password=control@123
host=10.10.0.20
database=controlplane
EOF
chmod 600 /root/.my-hosto.cnf
