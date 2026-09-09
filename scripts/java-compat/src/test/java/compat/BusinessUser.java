package compat;
import com.baomidou.mybatisplus.annotation.*;
import java.math.BigDecimal;
@TableName("business_user")
public class BusinessUser {
 @TableId(type=IdType.AUTO) private Long id;
 private String name;
 private BigDecimal amount;
 @Version private Integer version;
 @TableLogic private Integer deleted;
 public Long getId(){return id;} public void setId(Long x){id=x;}
 public String getName(){return name;} public void setName(String x){name=x;}
 public BigDecimal getAmount(){return amount;} public void setAmount(BigDecimal x){amount=x;}
 public Integer getVersion(){return version;} public void setVersion(Integer x){version=x;}
 public Integer getDeleted(){return deleted;} public void setDeleted(Integer x){deleted=x;}
}
