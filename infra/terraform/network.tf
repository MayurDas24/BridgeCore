# Network topology.
#
#                         INTERNET
#                            |
#                      Internet Gateway
#                            |
#                      Public subnets  ── ALB, NAT gateway
#                            |
#                     Private subnets   ── ECS Fargate tasks
#                            |
#                 ┌──────────┴──────────┐
#                RDS                  Redis
#            (private subnets, no route to the internet)
#
# The database and cache live in subnets with no route to an internet gateway.
# That is what makes "publicly_accessible = false" meaningful: even a
# misconfigured security group cannot expose them, because there is no path.

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = "${local.name}-vpc" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = { Name = "${local.name}-igw" }
}

# Subnets are carved deterministically from the VPC CIDR so adding an AZ never
# renumbers the existing ones (which would force a replace of everything in
# them).
resource "aws_subnet" "public" {
  count = var.availability_zone_count

  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, count.index)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true

  tags = {
    Name = "${local.name}-public-${count.index + 1}"
    Tier = "public"
  }
}

resource "aws_subnet" "private" {
  count = var.availability_zone_count

  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, count.index + 100)
  availability_zone = data.aws_availability_zones.available.names[count.index]

  tags = {
    Name = "${local.name}-private-${count.index + 1}"
    Tier = "private"
  }
}

# Fargate tasks need outbound access to pull images and reach AWS APIs, but
# must not be reachable from the internet: NAT gives them one without the
# other.
resource "aws_eip" "nat" {
  count = var.single_nat_gateway ? 1 : var.availability_zone_count

  domain = "vpc"
  tags   = { Name = "${local.name}-nat-eip-${count.index + 1}" }
}

resource "aws_nat_gateway" "main" {
  count = var.single_nat_gateway ? 1 : var.availability_zone_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = { Name = "${local.name}-nat-${count.index + 1}" }

  depends_on = [aws_internet_gateway.main]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${local.name}-public-rt" }
}

resource "aws_route_table_association" "public" {
  count = var.availability_zone_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# One private route table per AZ, so with per-AZ NAT gateways a NAT failure
# only affects its own AZ rather than all outbound traffic.
resource "aws_route_table" "private" {
  count = var.availability_zone_count

  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = var.single_nat_gateway ? aws_nat_gateway.main[0].id : aws_nat_gateway.main[count.index].id
  }

  tags = { Name = "${local.name}-private-rt-${count.index + 1}" }
}

resource "aws_route_table_association" "private" {
  count = var.availability_zone_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# S3 traffic (every usage export written by a worker) goes over a gateway
# endpoint rather than through NAT. That keeps export uploads inside the AWS
# network and removes them from the NAT data-processing bill, which for a
# bulk-upload workload is the difference that shows up on the invoice.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = aws_route_table.private[*].id

  tags = { Name = "${local.name}-s3-endpoint" }
}
