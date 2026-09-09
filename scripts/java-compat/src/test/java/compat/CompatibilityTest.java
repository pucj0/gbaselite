package compat;
import com.baomidou.mybatisplus.annotation.DbType;
import com.baomidou.mybatisplus.extension.plugins.MybatisPlusInterceptor;
import com.baomidou.mybatisplus.extension.plugins.inner.*;
import com.baomidou.mybatisplus.extension.plugins.pagination.Page;
import com.baomidou.mybatisplus.core.conditions.query.QueryWrapper;
import org.junit.jupiter.api.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.SpringBootConfiguration;
import org.springframework.boot.autoconfigure.EnableAutoConfiguration;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Bean;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionTemplate;
import org.mybatis.spring.annotation.MapperScan;
import javax.sql.DataSource;
import java.math.BigDecimal;
import java.sql.*;
import static org.junit.jupiter.api.Assertions.*;
@SpringBootTest(classes=CompatibilityTest.App.class,properties={"spring.datasource.url=${GBASE_TEST_URL}","spring.datasource.username=root","spring.datasource.password=compat-local-only","spring.datasource.hikari.maximum-pool-size=3","spring.datasource.hikari.minimum-idle=1","spring.main.banner-mode=off"})
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class CompatibilityTest {
 @SpringBootConfiguration @EnableAutoConfiguration @MapperScan("compat")
 static class App {
  @Bean BusinessService businessService(JdbcTemplate jdbc){return new BusinessService(jdbc);}
  @Bean MybatisPlusInterceptor plugins(){var x=new MybatisPlusInterceptor();x.addInnerInterceptor(new OptimisticLockerInnerInterceptor());x.addInnerInterceptor(new PaginationInnerInterceptor(DbType.MYSQL));return x;}
 }
 public static class BusinessService {
  private final JdbcTemplate jdbc;
  public BusinessService(JdbcTemplate jdbc){this.jdbc=jdbc;}
  @Transactional public void insert(String name, boolean fail){
   jdbc.update("INSERT INTO business_user(name,amount) VALUES(?,4.25)",name);
   if(fail)throw new IllegalStateException("rollback annotated transaction");
  }
 }
 @Autowired BusinessService service;
 @Autowired DataSource source;
 @Autowired JdbcTemplate jdbc;
 @Autowired PlatformTransactionManager transactions;
 @Autowired BusinessMapper mapper;
 @Test @Order(1) void preparedAndGeneratedKeys()throws Exception{
  jdbc.execute("CREATE TABLE business_user(id BIGINT PRIMARY KEY AUTO_INCREMENT,name VARCHAR(80),amount DECIMAL(20,2),version INT DEFAULT 0,deleted INT DEFAULT 0)");
  try(Connection c=source.getConnection()){
   c.setAutoCommit(false);
   try(PreparedStatement p=c.prepareStatement("INSERT INTO business_user(name,amount,version,deleted) VALUES(?,?,0,0)",Statement.RETURN_GENERATED_KEYS)){
    p.setString(1,"jdbc");p.setBigDecimal(2,new BigDecimal("9007199254740993.25"));assertEquals(1,p.executeUpdate());try(ResultSet keys=p.getGeneratedKeys()){assertTrue(keys.next());assertTrue(keys.getLong(1)>0);}
   }
   c.commit();c.setAutoCommit(true);
  }
  assertEquals(new BigDecimal("9007199254740993.25"),jdbc.queryForObject("SELECT amount FROM business_user WHERE name='jdbc'",BigDecimal.class));
 }
 @Test @Order(2) void springCommitRollback(){
  var tx=new TransactionTemplate(transactions);
  tx.executeWithoutResult(s->jdbc.update("INSERT INTO business_user(name,amount) VALUES('commit',1.25)"));
  assertThrows(IllegalStateException.class,()->tx.executeWithoutResult(s->{jdbc.update("INSERT INTO business_user(name,amount) VALUES('rollback',2.50)");throw new IllegalStateException("business failure");}));
  assertEquals(1,jdbc.queryForObject("SELECT COUNT(*) FROM business_user WHERE name='commit'",Integer.class));
  assertEquals(0,jdbc.queryForObject("SELECT COUNT(*) FROM business_user WHERE name='rollback'",Integer.class));
 }
 @Test @Order(3) void plusCrudPaginationAndOptimisticLock(){
  var u=new BusinessUser();u.setName("plus");u.setAmount(new BigDecimal("3.75"));u.setVersion(0);u.setDeleted(0);assertEquals(1,mapper.insert(u));assertNotNull(u.getId());
  var stale=mapper.selectById(u.getId());u.setName("updated");assertEquals(1,mapper.updateById(u));stale.setName("stale");assertEquals(0,mapper.updateById(stale));
  var page=mapper.selectPage(new Page<BusinessUser>(1,2),new QueryWrapper<BusinessUser>().orderByAsc("id"));assertTrue(page.getTotal()>=3);assertEquals(2,page.getRecords().size());
  assertEquals(1,mapper.deleteById(u.getId()));assertNull(mapper.selectById(u.getId()));
 }
 @Test @Order(5) void annotatedTransactionAndPoolReuse() throws Exception {
  service.insert("annotation-commit",false);
  assertThrows(IllegalStateException.class,()->service.insert("annotation-rollback",true));
  assertEquals(1,jdbc.queryForObject("SELECT COUNT(*) FROM business_user WHERE name='annotation-commit'",Integer.class));
  assertEquals(0,jdbc.queryForObject("SELECT COUNT(*) FROM business_user WHERE name='annotation-rollback'",Integer.class));
  try(Connection c=source.getConnection()) {
   c.setAutoCommit(false);
   try(Statement stmt=c.createStatement()){stmt.executeUpdate("INSERT INTO business_user(name,amount) VALUES('pool-rollback',1)");}
  }
  try(Connection c=source.getConnection()){assertTrue(c.getAutoCommit());}
  assertEquals(0,jdbc.queryForObject("SELECT COUNT(*) FROM business_user WHERE name='pool-rollback'",Integer.class));
 }
 @Test @Order(4) void ruoyiStyleRoleMenuJoin(){
  jdbc.execute("CREATE TABLE sys_role(role_id BIGINT PRIMARY KEY,role_name VARCHAR(40),status INT)");
  jdbc.execute("CREATE TABLE sys_user_role(user_id BIGINT,role_id BIGINT,KEY user_role(user_id,role_id))");
  jdbc.update("INSERT INTO sys_role VALUES(1,'admin',0),(2,'reader',0)");jdbc.update("INSERT INTO sys_user_role VALUES(1,1),(1,2)");
  assertEquals(2,jdbc.queryForList("SELECT r.role_id,r.role_name FROM sys_role r INNER JOIN sys_user_role ur ON r.role_id=ur.role_id WHERE ur.user_id=? ORDER BY r.role_id",1L).size());
  assertEquals(2,jdbc.queryForObject("SELECT COUNT(*) FROM sys_role r LEFT JOIN sys_user_role ur ON r.role_id=ur.role_id WHERE ur.user_id=?",Integer.class,1L));
 }
}
